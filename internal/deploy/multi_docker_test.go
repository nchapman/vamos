package deploy

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

// fakeDocker implements the external CLI boundary, not internal helpers. In
// particular network connect/disconnect take NETWORK CONTAINER, and inventory
// absence is distinct from a failed inspect. Lost responses happen AFTER the
// mutation, as they do when the client connection disappears.
type fakeVolume struct {
	Name, Driver, Scope, CreatedAt string
	Labels, Options                map[string]string
}

type fakeDocker struct {
	volumes     map[string]*fakeVolume
	t           *testing.T
	m           *Manager
	j           *journal
	images      map[string]*inspection
	containers  map[string]*inspection
	nextID      uint64
	net         *inspection
	calls       []string
	events      []string
	lose        map[string]int
	ignore      map[string]bool
	inspectHook func(*inspection)
	buildHook   func([]string, []byte) string
	beforeRun   func([]string)
}

func newFakeDocker(t *testing.T, m *Manager, j *journal) *fakeDocker {
	f := &fakeDocker{volumes: map[string]*fakeVolume{}, t: t, m: m, j: j, images: map[string]*inspection{}, containers: map[string]*inspection{}, lose: map[string]int{}, ignore: map[string]bool{}}
	f.net = &inspection{Driver: "bridge", Labels: map[string]string{"vamos.host": j.HostID, "vamos.app": j.App}}
	m.Run = f.run
	return f
}
func dockerJSON(v any) (process.Result, error) {
	b, err := json.Marshal(v)
	return process.Result{Stdout: b}, err
}
func fakeArg(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func (f *fakeDocker) response(key string) (process.Result, error) {
	if f.lose[key] > 0 {
		f.lose[key]--
		return process.Result{Stderr: []byte("sensitive-runtime-output")}, errors.New("sensitive-runtime-output")
	}
	return process.Result{}, nil
}
func (f *fakeDocker) endpoint(v *inspection, alias string) {
	if v.HostConfig.NetworkMode == "host" {
		if alias != "" {
			f.t.Fatal("host network cannot accept an alias")
		}
		v.NetworkSettings.Networks = map[string]json.RawMessage{"host": json.RawMessage(`{"Aliases":null}`)}
		return
	}
	aliases := []string{strings.TrimPrefix(v.Name, "/"), v.ID[:12]}
	if alias != "" {
		aliases = append(aliases, alias)
	}
	raw, _ := json.Marshal(map[string]any{"Aliases": aliases, "IPAddress": "172.25.0.2"})
	v.NetworkSettings.Networks = map[string]json.RawMessage{network(f.j): raw}
	if v.State.Running {
		v.NetworkSettings.Ports = v.HostConfig.PortBindings
	}
}
func (f *fakeDocker) run(_ context.Context, name string, a []string, input []byte) (process.Result, error) {
	f.t.Helper()
	if name != "docker" || len(a) == 0 {
		f.t.Fatalf("unexpected command: %s %v", name, a)
	}
	f.calls = append(f.calls, strings.Join(a, " "))
	switch a[0] {
	case "info":
		if !reflect.DeepEqual(a, []string{"info", "--format", "{{json .}}"}) {
			f.t.Fatalf("info signature: %v", a)
		}
		return dockerJSON(map[string]any{"MemoryLimit": true, "SwapLimit": true, "CpuCfsQuota": true, "CpuCfsPeriod": true, "NCPU": 8})
	case "buildx":
		if len(a) != 10 || !reflect.DeepEqual(a[:6], []string{"buildx", "build", "--load", "--network", "default", "--iidfile"}) || a[7] != "--file" || a[9] != "-" {
			f.t.Fatalf("build signature: %v", a)
		}
		id := "sha256:" + strings.Repeat("d", 64)
		if f.buildHook != nil {
			id = f.buildHook(a, input)
		}
		if f.images[id] == nil {
			f.images[id] = &inspection{ID: id}
		}
		if err := os.WriteFile(a[6], []byte(id), 0600); err != nil {
			f.t.Fatal(err)
		}
		f.events = append(f.events, "build")
		return f.response("build")
	case "image":
		if len(a) != 3 || a[1] != "inspect" {
			f.t.Fatalf("image signature: %v", a)
		}
		v := f.images[a[2]]
		if v == nil {
			return process.Result{}, errors.New("sensitive missing image")
		}
		return dockerJSON([]*inspection{v})
	case "volume":
		switch a[1] {
		case "ls":
			var b strings.Builder
			for n := range f.volumes {
				row, _ := json.Marshal(map[string]string{"Name": n})
				b.Write(row)
				b.WriteByte('\n')
			}
			return process.Result{Stdout: []byte(b.String())}, nil
		case "inspect":
			v := f.volumes[a[2]]
			if v == nil {
				return process.Result{}, errors.New("missing volume")
			}
			return dockerJSON([]*fakeVolume{v})
		case "create":
			n := a[len(a)-1]
			if f.volumes[n] == nil {
				v := &fakeVolume{Name: n, Driver: "local", Scope: "local", CreatedAt: "2026-06-01T00:00:00Z", Labels: map[string]string{}}
				for i := 2; i < len(a)-1; i++ {
					if a[i] == "--label" {
						k, val, _ := strings.Cut(a[i+1], "=")
						v.Labels[k] = val
						i++
					}
				}
				f.volumes[n] = v
			}
			f.events = append(f.events, "volume-create")
			return f.response("volume-create")
		}
	case "container":
		if len(a) >= 2 && a[1] == "ls" {
			filter := fakeArg(a, "--filter")
			if filter == "" && !reflect.DeepEqual(a, []string{"container", "ls", "--format", "{{json .}}", "--all"}) {
				f.t.Fatalf("container list signature: %v", a)
			}
			names := make([]string, 0, len(f.containers))
			for n, v := range f.containers {
				if filter != "" {
					matched := false
					for _, raw := range v.Mounts {
						var mount struct{ Name string }
						_ = json.Unmarshal(raw, &mount)
						if "volume="+mount.Name == filter {
							matched = true
						}
					}
					if !matched {
						continue
					}
				}
				names = append(names, n)
			}
			sort.Strings(names)
			var b strings.Builder
			for _, n := range names {
				row, _ := json.Marshal(map[string]any{"Names": n, "Platform": map[string]string{"Architecture": "amd64"}})
				b.Write(row)
				b.WriteByte('\n')
			}
			return process.Result{Stdout: []byte(b.String())}, nil
		}
		if len(a) != 3 || a[1] != "inspect" {
			f.t.Fatalf("container inspect signature: %v", a)
		}
		v := f.containers[a[2]]
		if v == nil {
			for _, candidate := range f.containers {
				if candidate.ID == a[2] {
					v = candidate
					break
				}
			}
		}
		if f.lose["inspect"] > 0 {
			return f.response("inspect")
		}
		if v == nil {
			return process.Result{}, errors.New("sensitive missing container")
		}
		if f.inspectHook != nil {
			f.inspectHook(v)
		}
		return dockerJSON([]*inspection{v})
	case "network":
		switch a[1] {
		case "ls":
			if !reflect.DeepEqual(a, []string{"network", "ls", "--format", "{{json .}}"}) {
				f.t.Fatalf("network list signature: %v", a)
			}
			if f.net == nil {
				return process.Result{}, nil
			}
			return dockerJSON(map[string]string{"Name": network(f.j)})
		case "inspect":
			if len(a) != 3 || a[2] != network(f.j) {
				f.t.Fatalf("network inspect signature: %v", a)
			}
			if f.net == nil {
				return process.Result{}, errors.New("network missing")
			}
			f.net.Containers = map[string]json.RawMessage{}
			for _, v := range f.containers {
				if v.State.Running && v.NetworkSettings.Networks[network(f.j)] != nil {
					f.net.Containers[v.ID] = json.RawMessage(`{}`)
				}
			}
			return dockerJSON([]*inspection{f.net})
		case "connect", "disconnect":
			alias := ""
			names := a[2:]
			if a[1] == "connect" && len(names) == 4 && names[0] == "--alias" {
				alias = names[1]
				names = names[2:]
			}
			if len(names) != 2 || names[0] != network(f.j) {
				f.t.Fatalf("network endpoint signature: %v", a)
			}
			v := f.containers[names[1]]
			if v == nil {
				f.t.Fatal("mutating missing container")
			}
			key := a[1]
			if alias != "" {
				key = "connect-alias"
			}
			f.events = append(f.events, key)
			if !f.ignore[key] {
				if key == "disconnect" {
					v.NetworkSettings.Networks = map[string]json.RawMessage{}
					v.NetworkSettings.Ports = nil
				} else {
					f.endpoint(v, alias)
				}
			}
			return f.response(key)
		}
	case "run", "create":
		if f.beforeRun != nil {
			f.beforeRun(a)
		}
		n := fakeArg(a, "--name")
		if n == "" || f.containers[n] != nil {
			f.t.Fatal("invalid/duplicate create")
		}
		id := ""
		pos := 0
		for i, x := range a {
			if imageID.MatchString(x) {
				id = x
				pos = i
				break
			}
		}
		im := f.images[id]
		if im == nil {
			f.t.Fatal("create image absent")
		}
		// Docker identities never repeat after removal. Using the live count
		// aliases surviving containers and makes ID-based inspection map-order dependent.
		f.nextID++
		v := &inspection{ID: fmt.Sprintf("%064x", f.nextID), Name: "/" + n}
		v.Config = im.Config
		v.Config.Image = id
		v.Config.Labels = map[string]string{}
		for key, value := range im.Config.Labels {
			v.Config.Labels[key] = value
		}
		env := map[string]string{}
		for _, x := range im.Config.Env {
			k, val, _ := strings.Cut(x, "=")
			env[k] = val
		}
		for i := 1; i < pos; i++ {
			switch a[i] {
			case "--cpus":
				cpus, err := strconv.ParseFloat(a[i+1], 64)
				if err != nil {
					f.t.Fatal(err)
				}
				v.HostConfig.NanoCPUs = int64(math.Round(cpus * 1e9))
				i++
			case "--memory", "--memory-swap":
				memory, err := strconv.ParseInt(a[i+1], 10, 64)
				if err != nil {
					f.t.Fatal(err)
				}
				if a[i] == "--memory" {
					v.HostConfig.Memory = memory
				} else {
					v.HostConfig.MemorySwap = memory
				}
				i++
			case "--mount":
				fields, err := csv.NewReader(strings.NewReader(a[i+1])).Read()
				if err != nil {
					f.t.Fatal(err)
				}
				values := map[string]string{}
				for _, field := range fields {
					k, val, _ := strings.Cut(field, "=")
					values[k] = val
				}
				if len(fields) != 4 || values["type"] != "volume" {
					f.t.Fatal("invalid mount", fields)
				}
				if _, ok := values["volume-nocopy"]; !ok {
					f.t.Fatal("missing nocopy")
				}
				source, target := values["source"], values["target"]
				if f.volumes[source] == nil {
					f.t.Fatal("mount references missing volume")
				}
				mount, _ := json.Marshal(map[string]any{"Type": "volume", "Name": source, "Destination": target, "Driver": "local", "RW": true})
				spec, _ := json.Marshal(map[string]any{"Type": "volume", "Source": source, "Target": target, "VolumeOptions": map[string]bool{"NoCopy": true}})
				v.Mounts = append(v.Mounts, mount)
				v.HostConfig.Mounts = append(v.HostConfig.Mounts, spec)
				i++
			case "--label":
				k, val, _ := strings.Cut(a[i+1], "=")
				v.Config.Labels[k] = val
				i++
			case "--env":
				k, val, _ := strings.Cut(a[i+1], "=")
				env[k] = val
				i++
			case "--env-file":
				b, err := os.ReadFile(a[i+1])
				if err != nil {
					f.t.Fatal(err)
				}
				for _, x := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					k, val, _ := strings.Cut(x, "=")
					env[k] = val
				}
				i++
			}
		}
		v.Config.Env = nil
		for k, val := range env {
			v.Config.Env = append(v.Config.Env, k+"="+val)
		}
		if pos+1 < len(a) {
			v.Config.Cmd = append([]string{}, a[pos+1:]...)
		}
		v.HostConfig.NetworkMode = fakeArg(a, "--network")
		v.HostConfig.RestartPolicy.Name = fakeArg(a, "--restart")
		for i, arg := range a {
			if arg != "--publish" {
				continue
			}
			ip, value, ok := strings.Cut(a[i+1], ":")
			if !ok || (ip != "127.0.0.1" && ip != "0.0.0.0") {
				f.t.Fatal("not explicit IPv4 publish", a[i+1])
			}
			p, err := config.ParsePort(value)
			if err != nil {
				f.t.Fatal(err)
			}
			if v.HostConfig.PortBindings == nil {
				v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{}
			}
			for offset := 0; offset < p.Count; offset++ {
				key := fmt.Sprintf("%d/%s", p.Container+offset, p.Protocol)
				v.HostConfig.PortBindings[key] = append(v.HostConfig.PortBindings[key], struct{ HostIP, HostPort string }{ip, strconv.Itoa(p.Host + offset)})
			}
		}
		if a[0] == "run" && f.publicPortConflict(v) {
			return process.Result{}, errors.New("host port already allocated")
		}
		v.State.Running = a[0] == "run"
		if v.State.Running {
			v.State.StartedAt = "2026-06-01T00:00:00Z"
		}
		f.endpoint(v, fakeArg(a, "--network-alias"))
		f.containers[n] = v
		f.events = append(f.events, a[0])
		return f.response(a[0])
	case "start", "update", "stop", "rm":
		n := a[len(a)-1]
		v := f.containers[n]
		if v == nil {
			f.t.Fatal("mutation of absent container")
		}
		f.events = append(f.events, a[0])
		switch a[0] {
		case "start":
			if f.publicPortConflict(v) {
				return process.Result{}, errors.New("host port already allocated")
			}
			if len(a) != 2 {
				f.t.Fatal(a)
			}
			if !f.ignore["start"] {
				v.State.StartedAt = "2026-06-01T00:00:00Z"
				v.State.Running = true
				v.NetworkSettings.Ports = v.HostConfig.PortBindings
			}
		case "update":
			if !reflect.DeepEqual(a, []string{"update", "--restart=no", n}) && !reflect.DeepEqual(a, []string{"update", "--restart=unless-stopped", n}) {
				f.t.Fatal(a)
			}
			if !f.ignore["update"] {
				v.HostConfig.RestartPolicy.Name = strings.TrimPrefix(a[1], "--restart=")
			}
		case "stop":
			if !reflect.DeepEqual(a, []string{"stop", "--time", "30", n}) {
				f.t.Fatal(a)
			}
			if !f.ignore["stop"] {
				v.State.Running = false
				v.State.Restarting = false
				v.NetworkSettings.Ports = nil
			}
		case "rm":
			if len(a) != 2 || v.State.Running || v.HostConfig.RestartPolicy.Name != "no" {
				f.t.Fatal("unsafe removal", a)
			}
			if !f.ignore["rm"] {
				delete(f.containers, n)
			}
		}
		return f.response(a[0])
	}
	f.t.Fatalf("unexpected docker command: %v", a)
	return process.Result{}, nil
}

func multiDockerFixture(t *testing.T, public bool) (*Manager, *journal, *release, *fakeDocker) {
	t.Helper()
	m, req := setup(t)
	m.PrivateObserve = 0
	m.HealthTimeout = time.Millisecond
	m.AllocatePort = func() (string, error) { return "23456", nil }
	service := "worker"
	s := config.Service{Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}, Env: map[string]string{"ROLE": "captured"}, Update: config.UpdateAuto}
	if public {
		service = "api"
		s.HTTP = &config.HTTP{Port: 8080, Health: "/ready"}
	}
	j := &journal{Status: Status{App: req.App, Operation: req.Operation, Phase: "updating", URL: "https://notes.example.com"}, HostID: strings.Repeat("a", 32), Config: &config.App{Name: req.App, Services: map[string]config.Service{service: s}}, Archive: req.Operation + ".context.tar", ServiceStates: map[string]*serviceState{}}
	r := &release{Service: service, Config: &s, Operation: req.Operation, Image: "sha256:" + strings.Repeat("d", 64), Container: containerName(j.App, service, req.Operation)}
	j.ServiceStates[service] = &serviceState{Phase: "starting", Candidate: r}
	if err := m.save(j); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker(t, m, j)
	im := &inspection{ID: r.Image}
	im.Config.Cmd = []string{"image-default"}
	im.Config.Env = []string{"DEFAULT=from-image"}
	im.Config.Labels = map[string]string{"org.opencontainers.image.title": "test-image"}
	f.images[r.Image] = im
	m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		f.events = append(f.events, "http")
		if req.URL.Host != "127.0.0.1:23456" || req.URL.Path != "/ready" {
			t.Fatalf("readiness did not use captured config: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	return m, j, r, f
}
func ensureMultiDocker(t *testing.T, m *Manager, j *journal, r *release) {
	t.Helper()
	if err := m.multiEnsureCandidate(context.Background(), j, r); err != nil {
		t.Fatal(err)
	}
}
func eventCount(f *fakeDocker, event string) int {
	n := 0
	for _, e := range f.events {
		if e == event {
			n++
		}
	}
	return n
}

func TestMultiDockerCreateCapturedAndLostResponse(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprint(public), func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, public)
			// Runtime must not project the latest desired app config onto this release.
			j.Config = app()
			f.lose["run"] = 1
			f.beforeRun = func(args []string) {
				if public {
					b, err := os.ReadFile(filepath.Join(m.dir(j.App), "state.json"))
					if err != nil {
						t.Fatal(err)
					}
					var saved journal
					if json.Unmarshal(b, &saved) != nil || saved.ServiceStates[r.Service].Candidate.PublishPort != "23456" {
						t.Fatal("port not journaled before create")
					}
					if fakeArg(args, "--network-alias") != "" {
						t.Fatal("public startup alias")
					}
				} else if fakeArg(args, "--publish") != "" || fakeArg(args, "--network-alias") != r.Service {
					t.Fatal("private create boundary")
				}
			}
			err := m.multiEnsureCandidate(context.Background(), j, r)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("lost create error not bounded", err)
			}
			ensureMultiDocker(t, m, j, r)
			if eventCount(f, "run") != 1 {
				t.Fatal("duplicate creation")
			}
			if public {
				if err := m.multiHealthy(context.Background(), j, r); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := m.multiObserve(context.Background(), j, r); err != nil {
					t.Fatal(err)
				}
			}
			f.containers[r.Container].State.Running = false
			ensureMultiDocker(t, m, j, r)
			if eventCount(f, "start") != 1 {
				t.Fatal("stopped candidate not adopted")
			}
			f.containers[r.Container].State.Running = false
			f.containers[r.Container].HostConfig.RestartPolicy.Name = "no"
			if m.multiEnsureCandidate(context.Background(), j, r) == nil || eventCount(f, "start") != 1 {
				t.Fatal("retired candidate restarted")
			}
		})
	}
}

func TestMultiDockerAliasRecovery(t *testing.T) {
	for _, lost := range []string{"", "disconnect", "connect-alias", "connect"} {
		t.Run("lost-"+lost, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, true)
			ensureMultiDocker(t, m, j, r)
			j.ServiceStates[r.Service].Phase = "aliasing"
			if lost == "connect" {
				f.containers[r.Container].NetworkSettings.Networks = nil
				f.containers[r.Container].NetworkSettings.Ports = nil
			}
			if lost != "" {
				f.lose[lost] = 1
			}
			err := m.multiAdmitAlias(context.Background(), j, r)
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal(err)
			}
			if err = m.multiAdmitAlias(context.Background(), j, r); err != nil {
				t.Fatal(err)
			}
			v := f.containers[r.Container]
			if !hasAlias(v, network(j), r.Service) || r.Port != "23456" {
				t.Fatal("alias recovery lost endpoint")
			}
			if _, err = m.multiInspect(context.Background(), j, r, false, true); err != nil {
				t.Fatal(err)
			}
			// No service-alias connect may precede first readiness.
			healthy := false
			for _, event := range f.events {
				if event == "http" {
					healthy = true
				}
				if event == "connect-alias" && !healthy {
					t.Fatal("startup DNS exposure")
				}
			}
		})
	}
}

func TestMultiDockerAliasPostHealthWithdrawal(t *testing.T) {
	for _, withdrawFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(withdrawFailure), func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, true)
			ensureMultiDocker(t, m, j, r)
			j.ServiceStates[r.Service].Phase = "aliasing"
			m.HTTP.Transport = transport(func(*http.Request) (*http.Response, error) {
				f.events = append(f.events, "http")
				code := 200
				if hasAlias(f.containers[r.Container], network(j), r.Service) {
					code = 503
					if withdrawFailure {
						f.ignore["disconnect"] = true
					}
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("sensitive-body"))}, nil
			})
			err := m.multiAdmitAlias(context.Background(), j, r)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal(err)
			}
			if withdrawFailure {
				if !strings.Contains(err.Error(), "unconfirmed") {
					t.Fatal("claimed withdrawal without proof", err)
				}
			} else if len(f.containers[r.Container].NetworkSettings.Networks) != 0 {
				t.Fatal("failed candidate still in DNS")
			}
		})
	}
}

func TestMultiDockerDetachedRequiresAliasing(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	ensureMultiDocker(t, m, j, r)
	v := f.containers[r.Container]
	v.NetworkSettings.Networks = nil
	v.NetworkSettings.Ports = nil
	for _, phase := range []string{"prepared", "starting", "promoting", "draining", "ready"} {
		j.ServiceStates[r.Service].Phase = phase
		if _, err := m.multiInspect(context.Background(), j, r, true, false); err == nil {
			t.Fatal("detachment accepted", phase)
		}
		if m.multiDiscard(context.Background(), j, r, true) == nil {
			t.Fatal("detached discard accepted", phase)
		}
	}
	j.ServiceStates[r.Service].Phase = "aliasing"
	if _, err := m.multiInspect(context.Background(), j, r, false, false); err == nil {
		t.Fatal("implicit detached inspection")
	}
	if _, err := m.multiInspect(context.Background(), j, r, true, false); err != nil {
		t.Fatal(err)
	}
	if err := m.multiDiscard(context.Background(), j, r, true); err != nil {
		t.Fatal(err)
	}
	if len(f.containers) != 0 {
		t.Fatal("detached candidate retained")
	}
}

func TestMultiDockerPrivateRestartObservation(t *testing.T) {
	for _, change := range []string{"restart-count", "started-at", "stopped", "restarting", "alias", "ports"} {
		t.Run(change, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, false)
			ensureMultiDocker(t, m, j, r)
			calls := 0
			f.inspectHook = func(v *inspection) {
				calls++
				// Each multiInspect includes one direct and one network endpoint inspect.
				if calls != 3 {
					return
				}
				switch change {
				case "restart-count":
					v.RestartCount++
				case "started-at":
					v.State.StartedAt = "changed"
				case "stopped":
					v.State.Running = false
				case "restarting":
					v.State.Restarting = true
				case "alias":
					f.endpoint(v, "")
				case "ports":
					v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "12345"}}}
				}
			}
			if m.multiObserve(context.Background(), j, r) == nil {
				t.Fatal("unstable private service accepted")
			}
		})
	}
}

func TestMultiDockerRetirementProofAndLostResponses(t *testing.T) {
	for _, failure := range []string{"", "lost-update", "lost-stop", "ignored-update", "ignored-stop"} {
		t.Run(failure, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, false)
			ensureMultiDocker(t, m, j, r)
			slot := j.ServiceStates[r.Service]
			slot.Current = r
			slot.Candidate = nil
			slot.CurrentRetired = true
			slot.Phase = "retiring"
			if strings.HasPrefix(failure, "lost-") {
				f.lose[strings.TrimPrefix(failure, "lost-")] = 1
			}
			if strings.HasPrefix(failure, "ignored-") {
				f.ignore[strings.TrimPrefix(failure, "ignored-")] = true
			}
			err := m.multiRetire(context.Background(), j, r)
			if failure != "" && err == nil {
				t.Fatal("retirement succeeded without proof")
			}
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal(err)
			}
			if failure == "ignored-update" && eventCount(f, "stop") != 0 {
				t.Fatal("stopped before disabling restart")
			}
			f.ignore = map[string]bool{}
			if err = m.multiRetire(context.Background(), j, r); err != nil {
				t.Fatal(err)
			}
			v := f.containers[r.Container]
			if v.State.Running || v.HostConfig.RestartPolicy.Name != "no" || eventCount(f, "start") != 0 {
				t.Fatal("retirement unsafe")
			}
			if m.multiEnsureCandidate(context.Background(), j, r) == nil || eventCount(f, "start") != 0 {
				t.Fatal("old worker restarted")
			}
		})
	}
}

func TestMultiDockerDiscardProof(t *testing.T) {
	for _, failure := range []string{"lost-rm", "ignored-rm", "inspect"} {
		t.Run(failure, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, true)
			ensureMultiDocker(t, m, j, r)
			switch failure {
			case "lost-rm":
				f.lose["rm"] = 1
			case "ignored-rm":
				f.ignore["rm"] = true
			case "inspect":
				f.lose["inspect"] = 1
			}
			if m.multiDiscard(context.Background(), j, r, false) == nil {
				t.Fatal("cleanup accepted without proof")
			}
			if failure == "inspect" && (eventCount(f, "update") != 0 || eventCount(f, "rm") != 0) {
				t.Fatal("inspect failure interpreted as absence")
			}
			f.ignore = map[string]bool{}
			if err := m.multiDiscard(context.Background(), j, r, false); err != nil {
				t.Fatal(err)
			}
			if err := m.multiDiscard(context.Background(), j, r, false); err != nil {
				t.Fatal("absence not idempotent", err)
			}
		})
	}
}

func TestMultiDockerInspectionDrift(t *testing.T) {
	changes := map[string]func(*inspection){
		"service":       func(v *inspection) { delete(v.Config.Labels, "vamos.service") },
		"name":          func(v *inspection) { v.Name += "-foreign" },
		"extra-label":   func(v *inspection) { v.Config.Labels["unexpected"] = "sensitive-value" },
		"operation":     func(v *inspection) { v.Config.Labels["vamos.operation"] = strings.Repeat("f", 32) },
		"image":         func(v *inspection) { v.Config.Image = "mutable:tag" },
		"environment":   func(v *inspection) { v.Config.Env = append(v.Config.Env, "EXTRA=sensitive-value") },
		"command":       func(v *inspection) { v.Config.Cmd = []string{"unexpected"} },
		"entrypoint":    func(v *inspection) { v.Config.Entrypoint = []string{"unexpected"} },
		"mount":         func(v *inspection) { v.Mounts = []json.RawMessage{json.RawMessage(`{}`)} },
		"bind":          func(v *inspection) { v.HostConfig.Binds = []string{"/tmp:/tmp"} },
		"privileged":    func(v *inspection) { v.HostConfig.Privileged = true },
		"host-network":  func(v *inspection) { v.HostConfig.NetworkMode = "host" },
		"extra-network": func(v *inspection) { v.NetworkSettings.Networks["other"] = json.RawMessage(`{}`) },
		"publish":       func(v *inspection) { p := v.HostConfig.PortBindings["8080/tcp"]; p[0].HostIP = "0.0.0.0" },
		"effective-port": func(v *inspection) {
			v.NetworkSettings.Ports = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "34567"}}}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, true)
			ensureMultiDocker(t, m, j, r)
			change(f.containers[r.Container])
			_, err := m.multiInspect(context.Background(), j, r, false, false)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("drift accepted or leaked", err)
			}
		})
	}
}

func TestMultiDockerNetworkIsolationAndAliasOwnership(t *testing.T) {
	for _, change := range []string{"ipv6", "ipv4-disabled", "routed", "icc-disabled", "unknown-option", "foreign-label", "prefix-name", "unrecorded", "foreign-alias"} {
		t.Run(change, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, false)
			ensureMultiDocker(t, m, j, r)
			v := f.containers[r.Container]
			switch change {
			case "ipv6":
				f.net.EnableIPv6 = true
			case "ipv4-disabled":
				b := false
				f.net.EnableIPv4 = &b
			case "routed":
				f.net.Options = map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "routed"}
			case "icc-disabled":
				f.net.Options = map[string]string{"com.docker.network.bridge.enable_icc": "false"}
			case "unknown-option":
				f.net.Options = map[string]string{"unrecognized": "enabled"}
			case "foreign-label":
				v.Config.Labels["vamos.app"] = "foreign"
			case "prefix-name":
				v.Name += "-not-owned"
			case "unrecorded":
				j.ServiceStates = map[string]*serviceState{}
			case "foreign-alias":
				f.endpoint(v, "other-service")
			}
			if m.ensureNetwork(context.Background(), j) == nil {
				t.Fatal("network drift accepted")
			}
		})
	}
}

func TestMultiDockerBuildAllBeforeRuntime(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	s := *r.Config
	s.Build = &config.Build{Context: "worker", Dockerfile: "worker/Dockerfile"}
	j.Config.Services["worker"] = s
	// Shared specs are built only once despite different runtime configuration.
	shared := *r.Config
	shared.Command = []string{"different-runtime"}
	j.Config.Services["shared"] = shared
	archives := map[string][]byte{}
	for key := range buildcontext.Builds(j.Config) {
		archives[key] = []byte("archive-" + key)
	}
	if len(archives) != 2 {
		t.Fatal("build deduplication failed")
	}
	f.buildHook = func(args []string, input []byte) string {
		if !strings.HasPrefix(string(input), "archive-") {
			t.Fatal("wrong build input")
		}
		id := "sha256:" + strings.Repeat("e", 64)
		if fakeArg(args, "--file") == "Dockerfile" {
			id = r.Image
		}
		return id
	}
	if err := m.multiBuild(context.Background(), j, archives); err != nil {
		t.Fatal(err)
	}
	if len(j.BuildImages) != 2 || eventCount(f, "build") != 2 || eventCount(f, "run") != 0 {
		t.Fatal("build/runtime barrier violated")
	}
	calls := len(f.calls)
	if err := m.multiBuild(context.Background(), j, archives); err != nil {
		t.Fatal(err)
	}
	if eventCount(f, "build") != 2 || len(f.calls)-calls != 2 {
		t.Fatal("resumed IDs not inspected exactly once")
	}
	f.images[r.Image].Config.Volumes = map[string]json.RawMessage{"/data": json.RawMessage(`{}`)}
	if m.multiBuild(context.Background(), j, archives) == nil || eventCount(f, "run") != 0 {
		t.Fatal("resumed image volumes accepted")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "buildx ") && (strings.Contains(call, "captured") || strings.Contains(call, "different-runtime")) {
			t.Fatal("runtime configuration entered build")
		}
	}
}

func TestMultiDockerLegacyIdentityAndDynamicPort(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	ensureMultiDocker(t, m, j, r)
	v := f.containers[r.Container]
	delete(f.containers, r.Container)
	oldService := r.Service
	r.Service = ""
	r.Container = containerName(j.App, "", r.Operation)
	v.Name = "/" + r.Container
	f.endpoint(v, "")
	delete(v.Config.Labels, "vamos.service")
	f.containers[r.Container] = v
	j.ServiceStates[oldService].Candidate = nil
	j.ServiceStates[oldService].Current = r
	r.Port = "23456"
	r.PublishPort = ""
	v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: ""}}}
	v.NetworkSettings.Ports = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "23456"}}}
	if _, err := m.multiInspect(context.Background(), j, r, false, true); err != nil {
		t.Fatal(err)
	}
	v.Config.Labels["vamos.service"] = ""
	if _, err := m.multiInspect(context.Background(), j, r, false, false); err == nil {
		t.Fatal("new service label accepted as legacy")
	}
}

func TestMultiDockerPinnedSecretsUsePrivateEnvFile(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	m.RuntimeDir = filepath.Join(m.Root, "runtime")
	if err := os.Mkdir(m.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	const pinned = "pinned-secret-canary"
	const latest = "latest-secret-canary"
	store := m.secretStore()
	if _, err := store.Stage(j.App, j.HostID, "token", strings.Repeat("1", 32), []byte(pinned)); err != nil {
		t.Fatal(err)
	}
	r.Config.Secrets = map[string]string{"TOKEN": "token"}
	state, err := store.Load(j.App, j.HostID)
	if err != nil {
		t.Fatal(err)
	}
	r.SecretRefs, err = state.Snapshot(r.Config.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Stage(j.App, j.HostID, "token", strings.Repeat("2", 32), []byte(latest)); err != nil {
		t.Fatal(err)
	}
	var envPath string
	f.beforeRun = func(args []string) {
		envPath = fakeArg(args, "--env-file")
		if !filepath.IsAbs(envPath) {
			t.Fatal("secret file must be absolute")
		}
		info, err := os.Stat(envPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("secret file permissions", err)
		}
		data, err := os.ReadFile(envPath)
		if err != nil || string(data) != "TOKEN="+pinned+"\n" {
			t.Fatal("wrong captured secret input", err)
		}
		for _, value := range []string{pinned, latest} {
			if strings.Contains(strings.Join(args, " "), value) {
				t.Fatal("secret in argv")
			}
		}
	}
	if err = m.save(j); err != nil {
		t.Fatal(err)
	}
	ensureMultiDocker(t, m, j, r)
	if _, err = os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("secret file retained")
	}
	if _, err = m.multiInspect(context.Background(), j, r, false, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.dir(j.App), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{pinned, latest} {
		if strings.Contains(string(raw), value) {
			t.Fatal("secret in journal")
		}
	}
	v := f.containers[r.Container]
	for i, env := range v.Config.Env {
		if strings.HasPrefix(env, "TOKEN=") {
			v.Config.Env[i] = "TOKEN=" + latest
		}
	}
	_, err = m.multiInspect(context.Background(), j, r, false, true)
	if err == nil || strings.Contains(err.Error(), pinned) || strings.Contains(err.Error(), latest) {
		t.Fatal("secret drift accepted or disclosed", err)
	}
}

func TestMultiDockerBuildPartialRecovery(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	second := *r.Config
	second.Build = &config.Build{Context: "second", Dockerfile: "second/Dockerfile"}
	j.Config.Services["second"] = second
	archives := map[string][]byte{}
	for key := range buildcontext.Builds(j.Config) {
		archives[key] = []byte("archive")
	}
	builds := 0
	f.buildHook = func(args []string, _ []byte) string {
		builds++
		if builds == 2 {
			f.lose["build"] = 1
		}
		return "sha256:" + strings.Repeat("e", 64)
	}
	// Build output is intentionally diagnostic: runtime secrets never enter builds.
	if err := m.multiBuild(context.Background(), j, archives); err == nil {
		t.Fatal(err)
	}
	if len(j.BuildImages) != 1 || len(f.containers) != 0 {
		t.Fatal("partial build barrier or checkpoint failed")
	}
	data, err := os.ReadFile(filepath.Join(m.dir(j.App), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved journal
	if json.Unmarshal(data, &saved) != nil || len(saved.BuildImages) != 1 {
		t.Fatal("build checkpoint not saved")
	}
	if err := m.multiBuild(context.Background(), j, archives); err != nil {
		t.Fatal(err)
	}
	if len(j.BuildImages) != 2 || builds != 3 || len(f.containers) != 0 {
		t.Fatal("resume rebuilt completed image or mutated runtime")
	}
}

func TestMultiDockerPromotionRequiresAdmittedAlias(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	ensureMultiDocker(t, m, j, r)
	j.ServiceStates[r.Service].Phase = "promoting"
	if err := m.multiHealthy(context.Background(), j, r); err == nil {
		t.Fatal("promotion accepted without alias")
	}
	if eventCount(f, "http") != 0 {
		t.Fatal("alias drift not detected before readiness")
	}
}

func TestMultiDockerEffectiveSnapshotCannotBecomeCandidate(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	j.Completed = &completedRelease{Operation: r.Operation, Services: map[string]*release{r.Service: r}}
	if m.multiEnsureCandidate(context.Background(), j, r) == nil {
		t.Fatal("effective snapshot resurrected")
	}
	if m.multiDiscard(context.Background(), j, r, false) == nil {
		t.Fatal("effective snapshot discarded")
	}
	if len(f.events) != 0 {
		t.Fatal("effective identity mutated")
	}
}

func TestMultiDockerAllowedPublicAliasOverlap(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	ensureMultiDocker(t, m, j, r)
	slot := j.ServiceStates[r.Service]
	slot.Phase = "aliasing"
	if err := m.multiAdmitAlias(context.Background(), j, r); err != nil {
		t.Fatal(err)
	}
	old := *r
	old.Operation = strings.Repeat("e", 32)
	old.Container = containerName(j.App, r.Service, old.Operation)
	old.PublishPort = "34567"
	old.Port = "34567"
	raw, _ := json.Marshal(f.containers[r.Container])
	var v inspection
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("copy inspection")
	}
	v.ID = strings.Repeat("f", 64)
	v.Name = "/" + old.Container
	v.Config.Labels["vamos.operation"] = old.Operation
	v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "34567"}}}
	f.endpoint(&v, r.Service)
	f.containers[old.Container] = &v
	slot.Current = &old
	if err := m.ensureNetwork(context.Background(), j); err != nil {
		t.Fatal("readiness-qualified alias overlap rejected", err)
	}
	if _, err := m.multiInspect(context.Background(), j, r, false, true); err != nil {
		t.Fatal(err)
	}
	// Snapshot retention alone cannot authorize a live old endpoint's alias.
	slot.Current = nil
	j.Completed = &completedRelease{Operation: old.Operation, Services: map[string]*release{old.Service: &old}}
	if m.ensureNetwork(context.Background(), j) == nil {
		t.Fatal("historical snapshot retained DNS authority")
	}
}

func TestMultiDockerPostAdmissionDriftWithdrawal(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, true)
			ensureMultiDocker(t, m, j, r)
			j.ServiceStates[r.Service].Phase = "aliasing"
			m.HTTP.Transport = transport(func(*http.Request) (*http.Response, error) {
				v := f.containers[r.Container]
				if hasAlias(v, network(j), r.Service) {
					if foreign {
						v.Config.Labels["vamos.app"] = "foreign"
					} else {
						v.NetworkSettings.Ports = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "34567"}}}
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			err := m.multiAdmitAlias(context.Background(), j, r)
			if err == nil {
				t.Fatal("post-admission drift accepted")
			}
			if foreign {
				if !strings.Contains(err.Error(), "unconfirmed") || eventCount(f, "disconnect") != 1 {
					t.Fatal("foreign endpoint mutated or withdrawal claimed", err)
				}
			} else if len(f.containers[r.Container].NetworkSettings.Networks) != 0 {
				t.Fatal("changed published endpoint not withdrawn")
			}
		})
	}
}

func TestMultiDockerPostAdmissionWithdrawalToleratesDaemonNormalization(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, true)
	ensureMultiDocker(t, m, j, r)
	j.ServiceStates[r.Service].Phase = "aliasing"
	m.HTTP.Transport = transport(func(*http.Request) (*http.Response, error) {
		v := f.containers[r.Container]
		if hasAlias(v, network(j), r.Service) {
			// Simulate dockerd restart normalizing nil HostConfig slices/bools
			// while the candidate restarts during post-admission readiness.
			off := false
			v.HostConfig.DNS = []string{}
			v.HostConfig.OomKillDisable = &off
			v.RestartCount++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	err := m.multiAdmitAlias(context.Background(), j, r)
	if err == nil || !strings.Contains(err.Error(), "endpoint withdrawn") || len(f.containers[r.Container].NetworkSettings.Networks) != 0 {
		t.Fatalf("expected endpoint withdrawal after daemon normalization, got err=%v networks=%v", err, f.containers[r.Container].NetworkSettings.Networks)
	}
}

// Model Docker's wildcard bind conflicts to catch accidental start-first
// replacements. Container ports need not be equal for host ports to collide.
func (f *fakeDocker) publicPortConflict(candidate *inspection) bool {
	for _, old := range f.containers {
		if old == candidate || !old.State.Running {
			continue
		}
		for key, bindings := range candidate.HostConfig.PortBindings {
			_, protocol, _ := strings.Cut(key, "/")
			for oldKey, oldBindings := range old.HostConfig.PortBindings {
				_, oldProtocol, _ := strings.Cut(oldKey, "/")
				if protocol != oldProtocol {
					continue
				}
				for _, b := range bindings {
					for _, prior := range oldBindings {
						if b.HostPort == prior.HostPort && (b.HostIP == "0.0.0.0" || prior.HostIP == "0.0.0.0") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func TestHostRuntimeInspectionRejectsNetworkAndPublishDrift(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	r.Config.NetworkMode = "host"
	j.Config.Services[r.Service] = *r.Config
	slot := j.ServiceStates[r.Service]
	slot.Phase = "prepared"
	ctx := context.Background()
	if err := m.multiEnsureRuntime(ctx, j, r, true); err != nil {
		t.Fatal(err)
	}
	slot.Phase = "starting"
	ensureMultiDocker(t, m, j, r)
	if err := m.multiObserve(ctx, j, r); err != nil {
		t.Fatal(err)
	}
	for name, drift := range map[string]func(*inspection){
		"mode":          func(v *inspection) { v.HostConfig.NetworkMode = network(j) },
		"detached":      func(v *inspection) { v.NetworkSettings.Networks = nil },
		"extra-network": func(v *inspection) { v.NetworkSettings.Networks[network(j)] = json.RawMessage(`{}`) },
		"alias":         func(v *inspection) { v.NetworkSettings.Networks["host"] = json.RawMessage(`{"Aliases":["worker"]}`) },
		"static-port": func(v *inspection) {
			v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{"80/tcp": nil}
		},
		"effective-port": func(v *inspection) {
			v.NetworkSettings.Ports = map[string][]struct{ HostIP, HostPort string }{"80/tcp": {{"0.0.0.0", "80"}}}
		},
		"publish-all": func(v *inspection) { v.HostConfig.PublishAllPorts = true },
	} {
		t.Run(name, func(t *testing.T) {
			old := f.containers[r.Container]
			b, _ := json.Marshal(old)
			var changed inspection
			if err := json.Unmarshal(b, &changed); err != nil {
				t.Fatal(err)
			}
			drift(&changed)
			f.containers[r.Container] = &changed
			defer func() { f.containers[r.Container] = old }()
			if err := m.multiObserve(ctx, j, r); err == nil {
				t.Fatal("observed drifted runtime")
			}
		})
	}
}
