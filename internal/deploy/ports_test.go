package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/secrets"
)

// Use the wire shape here as well as YAML tests: journals and API requests must
// retain the exact port intent through resume and rollback.
func portsFlowApp(t *testing.T, revision string) *config.App {
	t.Helper()
	a := multiApp()
	for name, s := range a.Services {
		b, _ := json.Marshal(s)
		var wire map[string]any
		_ = json.Unmarshal(b, &wire)
		if name == "web" {
			wire["Ports"] = []string{"9000:8080", "9001:8080/tcp"}
		} else {
			wire["Ports"] = []string{"3478:3478", "3478:3478/udp", "49160-49162:49160-49162/udp"}
		}
		wire["Env"] = map[string]string{"REVISION": revision}
		b, _ = json.Marshal(wire)
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		a.Services[name] = s
	}
	return a
}

func TestPortsFlowReplacementAndResume(t *testing.T) {
	for _, lost := range []string{"", "create", "update", "start", "stop"} {
		t.Run(lost, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			old := f.load()
			status := f.m.multiStatus(context.Background(), old, nil, false, "")
			if status.Services["backend"].Kind != "ports" || status.Services["web"].Kind != "public" {
				t.Fatal("incorrect exposure kind", status.Services)
			}
			starts := eventCount(f.docker, "start")
			if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("2", 32)); err != nil {
				t.Fatal(err)
			}
			if starts != eventCount(f.docker, "start") {
				t.Fatal("unchanged service restarted")
			}
			if lost != "" {
				f.docker.lose[lost] = 1
			}
			if _, err := f.deploy(portsFlowApp(t, "two"), strings.Repeat("3", 32)); err != nil {
				if _, retry := f.resume(strings.Repeat("3", 32)); retry != nil {
					t.Fatalf("deploy: %v; resume: %v", err, retry)
				}
			}
			if f.load().Phase != "ready" {
				t.Fatal("not ready")
			}
			for _, s := range old.ServiceStates {
				v := f.docker.containers[s.Current.Container]
				if v != nil && (v.State.Running || v.HostConfig.RestartPolicy.Name != "no") {
					t.Fatal("old runtime not retired")
				}
			}
			if lost != "" && f.docker.lose[lost] != 0 {
				t.Fatal("mutation not exercised")
			}
		})
	}
}

func TestPortsInspectExactBindings(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	for _, name := range []string{"web", "backend"} {
		ref := runtimeRefs(j.ServiceStates[name].Current)[0]
		original, _ := json.Marshal(f.docker.containers[ref.Container])
		cases := map[string]func(*inspection){
			"wrong IP": func(v *inspection) {
				for key, bs := range v.HostConfig.PortBindings {
					bs[0].HostIP = "::"
					v.HostConfig.PortBindings[key] = bs
					break
				}
			},
			"missing static": func(v *inspection) {
				for key := range v.HostConfig.PortBindings {
					delete(v.HostConfig.PortBindings, key)
					break
				}
			},
			"extra static": func(v *inspection) { v.HostConfig.PortBindings["123/udp"] = []portBinding{{"0.0.0.0", "123"}} },
			"duplicate": func(v *inspection) {
				for key, bs := range v.HostConfig.PortBindings {
					v.HostConfig.PortBindings[key] = append(bs, bs[0])
					break
				}
			},
			"missing effective": func(v *inspection) { v.NetworkSettings.Ports = nil },
			"extra effective":   func(v *inspection) { v.NetworkSettings.Ports["123/udp"] = []portBinding{{"0.0.0.0", "123"}} },
		}
		for label, mutate := range cases {
			t.Run(name+"/"+label, func(t *testing.T) {
				var v inspection
				_ = json.Unmarshal(original, &v)
				mutate(&v)
				f.docker.containers[ref.Container] = &v
				if _, err := f.m.instanceInspect(context.Background(), j, ref, false, true); err == nil {
					t.Fatal("accepted drift")
				}
			})
		}
		var v inspection
		_ = json.Unmarshal(original, &v)
		for _, bindings := range v.HostConfig.PortBindings {
			slices.Reverse(bindings)
		}
		v.NetworkSettings.Ports["9999/tcp"] = nil // image EXPOSE, not a publish
		f.docker.containers[ref.Container] = &v
		if _, err := f.m.instanceInspect(context.Background(), j, ref, false, true); err != nil {
			t.Fatal(err)
		}
		v.State.Restarting = true
		v.NetworkSettings.Ports = nil
		if _, err := f.m.instanceInspect(context.Background(), j, ref, false, true); err != nil {
			t.Fatal("restart backoff must remain inspectable for retirement", err)
		}
	}
}

func TestPortsTrackedConflictsLeaveOldRunning(t *testing.T) {
	for _, mode := range []string{"service", "app", "http"} {
		t.Run(mode, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			old := f.load()
			a := portsFlowApp(t, "two")
			switch mode {
			case "service":
				web := a.Services["web"]
				web.Ports = []string{"9100:8080"}
				a.Services["web"] = web
				backend := a.Services["backend"]
				backend.Ports = append(backend.Ports, "9000:90")
				a.Services["backend"] = backend
			case "app":
				// Rejection must happen before this second app reaches Docker networking.
				a.Name = "other"
				f.req.App = "other"
			case "http":
				backend := a.Services["backend"]
				backend.Ports = append(backend.Ports, old.ServiceStates["web"].Current.PublishPort+":90")
				a.Services["backend"] = backend
			}
			f.docker.events = nil
			if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("expected tracked conflict: %v", err)
			}
			if eventCount(f.docker, "stop") != 0 || eventCount(f.docker, "create") != 0 {
				t.Fatal("conflict mutated runtimes", f.docker.events)
			}
			for _, s := range old.ServiceStates {
				if !f.docker.containers[s.Current.Container].State.Running {
					t.Fatal("old stopped")
				}
			}
		})
	}
}

func TestPortsDockerConflictRecoveryAndRollback(t *testing.T) {
	for _, service := range []string{"web", "backend"} {
		t.Run(service, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			original := f.m.Run
			blocked := true
			f.m.Run = func(ctx context.Context, n string, args []string, input []byte) (process.Result, error) {
				if n == "docker" && args[0] == "start" {
					v := f.docker.containers[args[len(args)-1]]
					if v != nil && v.Config.Labels["vamos.service"] == service && blocked {
						return process.Result{Stderr: []byte("sensitive docker conflict")}, errors.New("sensitive docker conflict")
					}
				}
				return original(ctx, n, args, input)
			}
			if _, err := f.deploy(portsFlowApp(t, "two"), strings.Repeat("2", 32)); err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsafe or missing failure: %v", err)
			}
			j := f.load()
			slot := j.ServiceStates[service]
			if slot.Phase != "starting" || !slot.CurrentRetired || (service == "web" && !slot.Unavailable) {
				t.Fatalf("not recoverable: %+v", slot)
			}
			if service == "web" && !strings.Contains(string(f.live), "503") {
				t.Fatal("HTTP route not withdrawn")
			}
			blocked = false
			if _, err := f.resume(strings.Repeat("2", 32)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "", true), nil); err != nil {
				t.Fatal(err)
			}
			for _, s := range f.load().ServiceStates {
				if s.Current.Config.Env["REVISION"] != "one" || len(s.Current.Config.Ports) == 0 {
					t.Fatal("rollback lost ports/config")
				}
			}
		})
	}
}

func TestPortsAddRemoveAndV8(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	if _, err := f.deploy(replicaApp(2), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 8 {
		t.Fatal("expected v8 group")
	}
	f.req.HostIdentity = j.HostID
	f.req.Bindings = map[string]string{}
	for name, s := range j.ServiceStates {
		f.req.Bindings[name] = s.Incarnation
	}
	for i, a := range []*config.App{portsFlowApp(t, "one"), multiApp()} {
		if _, err := f.deploy(a, strings.Repeat(strconv.Itoa(i+2), 32)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range f.load().ServiceStates {
		if len(s.Current.Config.Ports) != 0 {
			t.Fatal("ports not removed")
		}
	}
}

func TestPortsClaimsPublicationSerializesApps(t *testing.T) {
	f := newMultiFlow(t)
	var mu sync.Mutex
	f.m.IngressLock = func(context.Context) (func(), error) { mu.Lock(); return mu.Unlock, nil }
	plans := []*journal{}
	for _, name := range []string{"first", "second"} {
		q := f.req
		q.App = name
		q.Config = portsFlowApp(t, "one")
		q.Config.Name = name
		j, err := multiPlan(q, &lifecycle.State{ID: strings.Repeat("a", 32)}, nil, &secrets.State{}, f.bundle(q.Config))
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, j)
	}
	results := make(chan error, 2)
	for _, j := range plans {
		go func() { results <- f.m.saveClaims(context.Background(), j) }()
	}
	successes := 0
	for range plans {
		if err := <-results; err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "conflict") {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected one reservation winner, got %d", successes)
	}
}

func TestHostnameClaimsPublication(t *testing.T) {
	f := newMultiFlow(t)
	var plans []*journal
	for _, name := range []string{"first", "first-backend"} {
		q := f.req
		q.Config = multiApp()
		q.App, q.Config.Name = name, name
		q.Config.Services["backend"] = q.Config.Services["web"]
		j, err := multiPlan(q, &lifecycle.State{ID: strings.Repeat("a", 32)}, nil, &secrets.State{}, f.bundle(q.Config))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.m.reserveHostnames(t.Context(), j); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, j)
	}
	if err := f.m.saveClaims(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.m.saveClaims(t.Context(), plans[1]); err == nil || !strings.Contains(err.Error(), "first-backend.example.com") {
		t.Fatalf("expected publication conflict, got %v", err)
	}
}

func TestPortsSupersedeAndVolumeCoexistence(t *testing.T) {
	for _, volume := range []bool{false, true} {
		t.Run(strconv.FormatBool(volume), func(t *testing.T) {
			f := newMultiFlow(t)
			a := portsFlowApp(t, "broken")
			if volume {
				for name, cfg := range a.Services {
					cfg.Volume = &config.Volume{Name: name + "-data", Mount: "/data"}
					a.Services[name] = cfg
				}
			}
			f.failHealth["web"] = true
			failed := strings.Repeat("1", 32)
			if _, err := f.deploy(a, failed); err == nil {
				t.Fatal("expected readiness failure")
			}
			candidate := f.load().ServiceStates["web"].Candidate
			v := f.docker.containers[candidate.Container]
			v.State.Restarting = true
			v.NetworkSettings.Ports = nil
			f.failHealth["web"] = false
			q := f.req
			q.Config = a
			q.Operation = strings.Repeat("2", 32)
			q.Supersede = failed
			if _, err := f.m.Execute(context.Background(), q, f.bundle(a)); err != nil {
				t.Fatal(err)
			}
			if f.docker.containers[candidate.Container] != nil {
				t.Fatal("superseded candidate remains")
			}
			for _, slot := range f.load().ServiceStates {
				if slot.DataMayHaveChanged != volume || slot.Current.ExecutionIntent != volume {
					t.Fatal("ports must not invent volume execution state")
				}
			}
		})
	}
}

func TestPortsReleaseCommandDoesNotPublish(t *testing.T) {
	f := newReleaseFlow(t)
	a := portsFlowApp(t, "one")
	a.Release = &config.Release{Service: "backend", Command: []string{"true"}}
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	record := f.m.hookRecord(f.load())
	if len(record.Config.Ports) != 0 {
		t.Fatal("release command retained port bindings")
	}
	for _, v := range f.docker.containers {
		if v.Config.Labels["vamos.run"] != "" && len(v.HostConfig.PortBindings) != 0 {
			t.Fatal("release command published ports")
		}
	}
}

func TestPortsFailedFirstDeploymentCanSupersedeWithoutPorts(t *testing.T) {
	f := newMultiFlow(t)
	f.failHealth["web"] = true
	failed := strings.Repeat("1", 32)
	if _, err := f.deploy(portsFlowApp(t, "broken"), failed); err == nil {
		t.Fatal("expected failure")
	}
	f.failHealth["web"] = false
	q := f.req
	q.Config = multiApp()
	q.Operation = strings.Repeat("2", 32)
	q.Supersede = failed
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err != nil {
		t.Fatal(err)
	}
	if f.load().Phase != "ready" {
		t.Fatal("supersede did not complete")
	}
}

func TestPortsHTTPAllocationConflictDoesNotRetireOld(t *testing.T) {
	f := newMultiFlow(t)
	a := portsFlowApp(t, "one")
	backend := a.Services["backend"]
	backend.HTTP = &config.HTTP{Port: 8081, Health: "/"}
	a.Services["backend"] = backend
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	f.m.AllocatePort = func() (string, error) { return j.ServiceStates["backend"].Current.PublishPort, nil }
	web := a.Services["web"]
	web.Env = map[string]string{"REVISION": "two"}
	a.Services["web"] = web
	f.docker.events = nil
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil {
		t.Fatal("HTTP allocation conflict accepted")
	}
	if eventCount(f.docker, "stop") != 0 || !f.docker.containers[j.ServiceStates["web"].Current.Container].State.Running {
		t.Fatal("old service stopped before tracked HTTP conflict refusal")
	}
}
