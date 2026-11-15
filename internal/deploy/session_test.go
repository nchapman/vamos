package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func sessionRequest() SessionRequest {
	return SessionRequest{Version: 1, Action: "exec", Host: lifecycle.Config{Name: "host", Domain: "example.com"}, App: "notes", Command: []string{"sh"}}
}

func TestValidateSessionRequest(t *testing.T) {
	validBinding := SessionBinding{HostIdentity: strings.Repeat("a", 32), Incarnation: strings.Repeat("b", 32)}
	for _, tc := range []struct {
		name string
		edit func(*SessionRequest)
	}{
		{"version", func(r *SessionRequest) { r.Version = 2 }},
		{"action", func(r *SessionRequest) { r.Action = "run" }},
		{"empty action", func(r *SessionRequest) { r.Action = "" }},
		{"app traversal", func(r *SessionRequest) { r.App = "../notes" }},
		{"empty app", func(r *SessionRequest) { r.App = "" }},
		{"long app", func(r *SessionRequest) { r.App = strings.Repeat("a", 64) }},
		{"service traversal", func(r *SessionRequest) { r.Service = "../web" }},
		{"long service", func(r *SessionRequest) { r.Service = strings.Repeat("a", 64) }},
		{"host", func(r *SessionRequest) { r.Host.Domain = "bad/domain" }},
		{"no command", func(r *SessionRequest) { r.Command = nil }},
		{"empty executable", func(r *SessionRequest) { r.Command = []string{"", "arg"} }},
		{"argument count", func(r *SessionRequest) { r.Command = append([]string{"sh"}, make([]string, 256)...) }},
		{"argument length", func(r *SessionRequest) { r.Command = []string{strings.Repeat("a", 8193)} }},
		{"total length", func(r *SessionRequest) {
			r.Command = []string{strings.Repeat("a", 8192), strings.Repeat("b", 8192), strings.Repeat("c", 8192), strings.Repeat("d", 8192), "x"}
		}},
		{"NUL executable", func(r *SessionRequest) { r.Command = []string{"sh\x00"} }},
		{"NUL argument", func(r *SessionRequest) { r.Command = []string{"sh", "a\x00b"} }},
		{"invalid UTF8", func(r *SessionRequest) { r.Command = []string{"sh", string([]byte{0xff})} }},
		{"exec candidate", func(r *SessionRequest) { r.Candidate = true }},
		{"exec tail", func(r *SessionRequest) { r.Tail = 1 }},
		{"exec negative tail", func(r *SessionRequest) { r.Tail = -1 }},
		{"tty without stdin", func(r *SessionRequest) { r.TTY = true }},
		{"binding name", func(r *SessionRequest) { r.Bindings = map[string]SessionBinding{"../web": validBinding} }},
		{"binding empty host", func(r *SessionRequest) {
			r.Bindings = map[string]SessionBinding{"web": {Incarnation: validBinding.Incarnation}}
		}},
		{"binding invalid incarnation", func(r *SessionRequest) {
			r.Bindings = map[string]SessionBinding{"web": {HostIdentity: validBinding.HostIdentity, Incarnation: strings.Repeat("G", 32)}}
		}},
		{"too many bindings", func(r *SessionRequest) {
			r.Bindings = map[string]SessionBinding{}
			for i := 0; i < 65; i++ {
				r.Bindings[fmt.Sprintf("s%d", i)] = validBinding
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sessionRequest()
			tc.edit(&r)
			if err := ValidateSessionRequest(r); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*SessionRequest)
	}{
		{"zero tail", func(r *SessionRequest) { r.Tail = 0 }},
		{"negative tail", func(r *SessionRequest) { r.Tail = -1 }},
		{"excess tail", func(r *SessionRequest) { r.Tail = 1001 }},
		{"command", func(r *SessionRequest) { r.Command = []string{"sh"} }},
		{"stdin", func(r *SessionRequest) { r.Stdin = true }},
		{"tty", func(r *SessionRequest) { r.TTY = true }},
	} {
		t.Run("logs "+tc.name, func(t *testing.T) {
			r := sessionRequest()
			r.Action, r.Command, r.Tail = "logs", nil, 100
			tc.edit(&r)
			if err := ValidateSessionRequest(r); err == nil {
				t.Fatal("accepted invalid logs request")
			}
		})
	}
	for _, edit := range []func(*SessionRequest){
		func(r *SessionRequest) {},
		func(r *SessionRequest) { r.Stdin = true },
		func(r *SessionRequest) { r.Stdin, r.TTY = true, true },
		func(r *SessionRequest) {
			r.Command = []string{"echo", "", "a b", ";$(touch /tmp/not-executed)", "\n", "日本語"}
		},
		func(r *SessionRequest) { r.Command = append([]string{"sh"}, make([]string, 255)...) },
		func(r *SessionRequest) {
			r.Command = []string{strings.Repeat("a", 8192), strings.Repeat("b", 8192), strings.Repeat("c", 8192), strings.Repeat("d", 8192)}
		},
		func(r *SessionRequest) { r.Action, r.Command, r.Tail, r.Candidate = "logs", nil, 1, true },
		func(r *SessionRequest) { r.Action, r.Command, r.Tail = "logs", nil, 1000 },
		func(r *SessionRequest) {
			r.Bindings = map[string]SessionBinding{}
			for i := 0; i < 64; i++ {
				r.Bindings[fmt.Sprintf("s%d", i)] = validBinding
			}
		},
	} {
		r := sessionRequest()
		edit(&r)
		before, _ := json.Marshal(r)
		if err := ValidateSessionRequest(r); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(r)
		if string(before) != string(after) {
			t.Fatal("validation changed arguments or metadata")
		}
	}
}

func newSessionFlow(t *testing.T) *multiFlow {
	t.Helper()
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSessionSelectionAndImmutableIdentity(t *testing.T) {
	f := newSessionFlow(t)
	j := f.load()
	r := sessionRequest()
	for _, name := range []string{"", "web", "backend"} {
		r.Service = name
		target, err := f.m.ResolveSession(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if name == "" {
			name = "web"
		}
		rel := j.ServiceStates[name].Current
		want := SessionTarget{HostIdentity: j.HostID, App: j.App, Service: name, Incarnation: serviceIncarnation(j, name), Operation: rel.Operation, ContainerID: f.docker.containers[rel.Container].ID, Container: rel.Container}
		if target != want {
			t.Fatalf("target = %+v, want %+v", target, want)
		}
		// Changing the immutable ID without changing a display name is visible
		// to the caller's whole-target attach comparison.
		v := f.docker.containers[rel.Container]
		originalEndpoint := v.NetworkSettings.Networks[network(j)]
		v.ID = strings.Repeat("f", 64)
		v.NetworkSettings.Networks[network(j)] = json.RawMessage(fmt.Sprintf(`{"Aliases":[%q,%q,%q]}`, name, rel.Container, v.ID[:12]))
		next, err := f.m.ResolveSession(context.Background(), r)
		if err != nil || next == target || next.Container != target.Container {
			t.Fatalf("replaced identity not detected: %+v %v", next, err)
		}
		v.ID = target.ContainerID
		v.NetworkSettings.Networks[network(j)] = originalEndpoint
	}
	r.Service = "removed"
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("resolved missing service")
	}
}

func TestSessionDefaultSelectionWithoutWeb(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newMultiFlow(t)
			a := multiApp()
			delete(a.Services, "web")
			if count == 2 {
				a.Services["worker"] = a.Services["backend"]
			}
			if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			target, err := f.m.ResolveSession(context.Background(), sessionRequest())
			if count == 1 && (err != nil || target.Service != "backend") {
				t.Fatalf("sole service: %+v %v", target, err)
			}
			if count == 2 && (err == nil || !strings.Contains(err.Error(), "ambiguous")) {
				t.Fatalf("ambiguous selection: %v", err)
			}
		})
	}
}

func TestSessionBindings(t *testing.T) {
	f := newSessionFlow(t)
	j := f.load()
	good := SessionBinding{HostIdentity: j.HostID, Incarnation: serviceIncarnation(j, "web")}
	for _, tc := range []struct {
		name      string
		bindings  map[string]SessionBinding
		wantError bool
	}{
		{"optional", nil, false},
		{"selected valid", map[string]SessionBinding{"web": good}, false},
		{"unselected stale", map[string]SessionBinding{"web": good, "backend": {HostIdentity: strings.Repeat("e", 32), Incarnation: strings.Repeat("f", 32)}}, false},
		{"selected host stale", map[string]SessionBinding{"web": {HostIdentity: strings.Repeat("e", 32), Incarnation: good.Incarnation}}, true},
		{"selected incarnation stale", map[string]SessionBinding{"web": {HostIdentity: good.HostIdentity, Incarnation: strings.Repeat("f", 32)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sessionRequest()
			r.Bindings = tc.bindings
			_, err := f.m.ResolveSession(context.Background(), r)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSessionRecordedIncarnation(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	incarnation := j.ServiceStates["web"].Incarnation
	if j.Version != 7 || incarnation == "" {
		t.Fatal("fixture has no recorded incarnation")
	}
	r := sessionRequest()
	r.Bindings = map[string]SessionBinding{"web": {HostIdentity: j.HostID, Incarnation: incarnation}}
	target, err := f.m.ResolveSession(context.Background(), r)
	if err != nil || target.Incarnation != incarnation {
		t.Fatalf("recorded incarnation: %+v %v", target, err)
	}
}

func TestSessionRejectsUnsafeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*multiFlow, *inspection)
		logsOK bool
	}{
		{"stopped", func(_ *multiFlow, v *inspection) { v.State.Running = false }, true},
		{"paused", func(_ *multiFlow, v *inspection) { v.State.Paused = true }, true},
		{"restarting", func(_ *multiFlow, v *inspection) { v.State.Restarting = true }, true},
		{"dead", func(_ *multiFlow, v *inspection) { v.State.Dead = true }, true},
		{"retired policy", func(_ *multiFlow, v *inspection) { v.HostConfig.RestartPolicy.Name = "no" }, true},
		{"missing", func(f *multiFlow, v *inspection) { delete(f.docker.containers, strings.TrimPrefix(v.Name, "/")) }, false},
		{"empty id", func(_ *multiFlow, v *inspection) { v.ID = "" }, false},
		{"short id", func(_ *multiFlow, v *inspection) { v.ID = "abcdef123456" }, false},
		{"foreign host", func(_ *multiFlow, v *inspection) { v.Config.Labels["vamos.host"] = strings.Repeat("f", 32) }, false},
		{"foreign service", func(_ *multiFlow, v *inspection) { v.Config.Labels["vamos.service"] = "other" }, false},
		{"foreign operation", func(_ *multiFlow, v *inspection) { v.Config.Labels["vamos.operation"] = strings.Repeat("f", 32) }, false},
		{"foreign mount", func(_ *multiFlow, v *inspection) {
			v.Mounts = []json.RawMessage{json.RawMessage(`{"Type":"bind","Source":"/etc","Destination":"/data"}`)}
		}, false},
		{"foreign network", func(_ *multiFlow, v *inspection) {
			v.NetworkSettings.Networks = map[string]json.RawMessage{"other": json.RawMessage(`{}`)}
		}, false},
		{"foreign network owner", func(f *multiFlow, _ *inspection) { f.docker.net.Labels["vamos.host"] = strings.Repeat("f", 32) }, false},
		{"image", func(_ *multiFlow, v *inspection) { v.Config.Image = "sha256:" + strings.Repeat("f", 64) }, false},
		{"privileged", func(_ *multiFlow, v *inspection) { v.HostConfig.Privileged = true }, false},
		{"environment", func(_ *multiFlow, v *inspection) { v.Config.Env = append(v.Config.Env, "UNRECORDED=secret") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionFlow(t)
			j := f.load()
			v := f.docker.containers[j.ServiceStates["web"].Current.Container]
			tc.edit(f, v)
			r := sessionRequest()
			if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
				t.Fatal("exec admitted unsafe target")
			}
			r.Action, r.Command, r.Tail = "logs", nil, 100
			_, err := f.m.ResolveSession(context.Background(), r)
			if (err == nil) != tc.logsOK {
				t.Fatalf("logs error = %v, want success %v", err, tc.logsOK)
			}
		})
	}
}

func TestSessionCandidateAndCapturedOperation(t *testing.T) {
	f := newSessionFlow(t)
	f.failHealth["web"] = true
	if _, err := f.deploy(changedMultiApp("two"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected interrupted update")
	}
	j := f.load()
	slot := j.ServiceStates["web"]
	if slot.Candidate == nil || slot.Current.Operation == j.Operation {
		t.Fatal("fixture did not retain old Current and new Candidate")
	}
	r := sessionRequest()
	target, err := f.m.ResolveSession(context.Background(), r)
	if err != nil || target.Operation != slot.Current.Operation || target.Container != slot.Current.Container {
		t.Fatalf("exec retargeted: %+v %v", target, err)
	}
	r.Action, r.Command, r.Tail, r.Candidate = "logs", nil, 100, true
	target, err = f.m.ResolveSession(context.Background(), r)
	if err != nil || !target.Candidate || target.Operation != slot.Candidate.Operation || target.ContainerID != f.docker.containers[slot.Candidate.Container].ID {
		t.Fatalf("candidate logs: %+v %v", target, err)
	}
}

func TestSessionRejectsRetiredCurrentWithoutRetargeting(t *testing.T) {
	f := newSessionFlow(t)
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			f.docker.lose["run"] = 1
		}
	}
	if _, err := f.deploy(changedMultiApp("two"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected interrupted update")
	}
	j := f.load()
	slot := j.ServiceStates["backend"]
	if !slot.CurrentRetired || slot.Candidate == nil {
		t.Fatal("fixture did not retire current")
	}
	// A running container cannot override durable retirement intent.
	v := f.docker.containers[slot.Current.Container]
	v.State.Running, v.HostConfig.RestartPolicy.Name = true, "unless-stopped"
	r := sessionRequest()
	r.Service = "backend"
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired exec: %v", err)
	}
	r.Action, r.Command, r.Tail = "logs", nil, 100
	target, err := f.m.ResolveSession(context.Background(), r)
	if err != nil || target.Operation != slot.Current.Operation {
		t.Fatalf("retired logs: %+v %v", target, err)
	}
}

func TestSessionAdmissionLockAndReadOnlyResolution(t *testing.T) {
	f := newSessionFlow(t)
	p := filepath.Join(f.m.dir(f.req.App), "state.json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(f.m.path(lifecycle.LockPath))
	defer lock.Close()
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	r := sessionRequest()
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil || !strings.Contains(err.Error(), "mutation") {
		t.Fatalf("mutation admitted: %v", err)
	}
	r.Action, r.Command, r.Tail = "logs", nil, 100
	if _, err := f.m.ResolveSession(context.Background(), r); err != nil {
		t.Fatal("observational logs blocked:", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	baseRun := f.m.Run
	calls := 0
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		calls++
		if name != "docker" || len(args) != 3 || args[1] != "inspect" || len(input) != 0 {
			t.Fatalf("resolution performed non-inspection: %s %v", name, args)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("unbounded inspection")
		}
		if ok, err := lock.TryLock(); err != nil || ok {
			t.Fatalf("exec inspection not locked: %v %v", ok, err)
		}
		return baseRun(ctx, name, args, input)
	}
	if _, err := f.m.ResolveSession(context.Background(), sessionRequest()); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("no inspection")
	}
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("lock not released: %v %v", ok, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	f.m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, errors.New("inspection failure")
	}
	if _, err := f.m.ResolveSession(context.Background(), sessionRequest()); err == nil {
		t.Fatal("ignored inspection error")
	}
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("error leaked lock: %v %v", ok, err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("resolution mutated/upgraded journal")
	}
}

func TestSessionHostAndCancellation(t *testing.T) {
	f := newSessionFlow(t)
	r := sessionRequest()
	r.Host.Name = "different-host"
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("accepted stale host config")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.m.ResolveSession(ctx, sessionRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	h, err := lifecycle.ReadState(f.m.path(lifecycle.StatePath))
	if err != nil {
		t.Fatal(err)
	}
	h.Phase = "failed"
	b, _ := json.Marshal(h)
	if err := atomic(f.m.path(lifecycle.StatePath), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ResolveSession(context.Background(), sessionRequest()); err == nil {
		t.Fatal("accepted host that is not ready")
	}
	h.Phase = "ready"
	h.ID = strings.Repeat("f", 32)
	b, _ = json.Marshal(h)
	if err := atomic(f.m.path(lifecycle.StatePath), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ResolveSession(context.Background(), sessionRequest()); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("accepted foreign journal: %v", err)
	}
}

func TestSessionMaintenanceAdmission(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			f := newSessionFlow(t)
			j := f.load()
			base := f.m.Run
			inventory := newMultiMaintenanceInventory(t)
			inventory.j = j
			inventory.net.Name = network(j)
			inventory.net.Labels = map[string]string{"vamos.host": j.HostID, "vamos.app": j.App}
			for _, slot := range j.ServiceStates {
				inventory.add(slot.Current).State.Running = true
			}
			f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name == "docker" {
					return inventory.m.Run(ctx, name, args, input)
				}
				return base(ctx, name, args, input)
			}
			if scoped {
				maintenanceRun := f.m.Run
				f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
					if name == "docker" && len(args) > 1 && args[0] == "container" && args[1] == "update" {
						return process.Result{}, errors.New("interrupt service removal before retiring container")
					}
					return maintenanceRun(ctx, name, args, input)
				}
				q := Request{Version: 1, Action: "service-remove", App: j.App, Host: f.req.Host, Operation: strings.Repeat("3", 32), Service: "backend", Apply: true}
				if _, err := f.m.executeServiceRemoval(context.Background(), q, j); err == nil {
					t.Fatal("expected interrupted removal")
				}
				f.m.Run = base
				j = f.load()
				if j.Maintenance == nil || j.Maintenance.Service != "backend" {
					t.Fatal("removal did not capture maintenance")
				}
			} else {
				plan, err := f.m.planMaintenance(context.Background(), j, "cleanup", strings.Repeat("3", 32))
				if err != nil {
					t.Fatal(err)
				}
				j.Maintenance, j.Phase = plan, "cleaning"
				if err := f.m.save(j); err != nil {
					t.Fatal(err)
				}
			}
			f.m.Run = base
			r := sessionRequest()
			_, err := f.m.ResolveSession(context.Background(), r)
			if scoped && err != nil {
				t.Fatal("unselected maintenance blocked web:", err)
			}
			if !scoped && (err == nil || !strings.Contains(err.Error(), "maintenance")) {
				t.Fatalf("global maintenance admission: %v", err)
			}
			r.Service = "backend"
			if _, err := f.m.ResolveSession(context.Background(), r); err == nil || !strings.Contains(err.Error(), "maintenance") {
				t.Fatalf("selected maintenance admission: %v", err)
			}
			r.Action, r.Command, r.Tail = "logs", nil, 100
			if _, err := f.m.ResolveSession(context.Background(), r); err != nil {
				t.Fatal("maintenance logs blocked:", err)
			}
		})
	}
}

func TestSessionVolumesAndUnavailable(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	r := sessionRequest()
	if _, err := f.m.ResolveSession(context.Background(), r); err != nil {
		t.Fatal("valid volume rejected:", err)
	}
	j := f.load()
	v := f.docker.containers[j.ServiceStates["web"].Current.Container]
	mounts := v.Mounts
	v.Mounts = nil
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("missing declared volume accepted")
	}
	v.Mounts = mounts
	f.failHealth["web"] = true
	if _, err := f.deploy(volumeFlowApp("two"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected interrupted volume deployment")
	}
	j = f.load()
	if !j.ServiceStates["web"].Unavailable {
		t.Fatal("fixture has no unavailable Current")
	}
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unavailable exec: %v", err)
	}
}

func TestSessionMissingCurrentNeverFallsBackToCandidate(t *testing.T) {
	f := newMultiFlow(t)
	f.failHealth["web"] = true
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err == nil {
		t.Fatal("expected failed first deployment")
	}
	r := sessionRequest()
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("exec fell back to candidate")
	}
	r.Action, r.Command, r.Tail = "logs", nil, 100
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("logs fell back to candidate")
	}
	r.Candidate = true
	if target, err := f.m.ResolveSession(context.Background(), r); err != nil || !target.Candidate {
		t.Fatalf("explicit candidate logs: %+v %v", target, err)
	}
}

func TestSessionLegacyWeb(t *testing.T) {
	f, request := flowSetup(t)
	j := f.execute(request, "legacy", false)
	v := f.containers[j.Current.Container]
	v.ID = strings.Repeat("d", 64)
	p := filepath.Join(f.m.dir(j.App), "state.json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	r := sessionRequest()
	r.Bindings = map[string]SessionBinding{"web": {HostIdentity: j.HostID, Incarnation: serviceIncarnation(j, "web")}}
	target, err := f.m.ResolveSession(context.Background(), r)
	if err != nil || target.Service != "web" || target.ContainerID != v.ID || target.Operation != j.Current.Operation || target.Incarnation != serviceIncarnation(j, "web") {
		t.Fatalf("legacy target: %+v %v", target, err)
	}
	r.Bindings["web"] = SessionBinding{HostIdentity: j.HostID, Incarnation: strings.Repeat("f", 32)}
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("legacy stale binding accepted")
	}
	r.Bindings = nil
	r.Service = "backend"
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("legacy non-web selection accepted")
	}
	r.Service = ""
	v.State.Running = false
	if _, err := f.m.ResolveSession(context.Background(), r); err == nil {
		t.Fatal("legacy stopped exec accepted")
	}
	r.Action, r.Command, r.Tail = "logs", nil, 100
	if _, err := f.m.ResolveSession(context.Background(), r); err != nil {
		t.Fatal("legacy stopped logs blocked:", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("legacy journal upgraded or mutated")
	}
}

func TestSessionJSONMetadata(t *testing.T) {
	r := sessionRequest()
	r.Bindings = map[string]SessionBinding{"web": {HostIdentity: strings.Repeat("a", 32), Incarnation: strings.Repeat("b", 32)}}
	r.Command = []string{"printf", "", "$HOME; echo not-a-shell"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip SessionRequest
	if err := json.Unmarshal(b, &roundtrip); err != nil || !reflect.DeepEqual(r, roundtrip) {
		t.Fatalf("literal argument roundtrip: %s %v", b, err)
	}
	target := SessionTarget{HostIdentity: "host", App: "notes", Service: "web", Incarnation: "inc", Operation: "op", ContainerID: "id", Container: "display", Candidate: true}
	b, _ = json.Marshal(target)
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"host_identity", "app", "service", "incarnation", "operation", "container_id", "container", "candidate"}
	if len(fields) != len(want) {
		t.Fatalf("unexpected metadata: %s", b)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s in %s", key, b)
		}
	}
}
