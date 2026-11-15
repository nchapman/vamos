package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func testResources(cpus float64, memory int64) *config.Resources {
	return &config.Resources{CPUs: &cpus, MemoryMB: &memory}
}

func TestResourcesAdmissionPreservesExistingRuntimes(t *testing.T) {
	for _, cpu := range []float64{0.5, 8} {
		t.Run(fmt.Sprint(cpu), func(t *testing.T) {
			f := newMultiFlow(t)
			a := multiApp()
			if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			before := f.load()
			routes := string(f.live)
			s := a.Services["backend"]
			s.Resources = testResources(cpu, 512)
			a.Services["backend"] = s
			old := f.m.Run
			f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name == "docker" && args[0] == "info" {
					return dockerJSON(map[string]any{"MemoryLimit": cpu != 0.5, "SwapLimit": true, "CpuCfsQuota": true, "CpuCfsPeriod": true, "NCPU": 4})
				}
				return old(ctx, name, args, input)
			}
			if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil {
				t.Fatal("unsupported resources accepted")
			}
			for name, slot := range before.ServiceStates {
				v := f.docker.containers[slot.Current.Container]
				if !v.State.Running || v.HostConfig.RestartPolicy.Name != "unless-stopped" {
					t.Fatalf("resource admission failure retired %s", name)
				}
			}
			if string(f.live) != routes {
				t.Fatal("resource admission changed routes")
			}
		})
	}
}

func TestResourcesRuntimeArgs(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	r.Config.Resources = testResources(0.501, 512)
	if err := m.multiEnsureCandidate(context.Background(), j, r); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.calls, "\n")
	for _, want := range []string{"--cpus 0.501", "--memory 536870912", "--memory-swap 536870912"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %s in %s", want, calls)
		}
	}
}

func TestResourcesUnsupportedBeforeExecution(t *testing.T) {
	for _, field := range []string{"MemoryLimit", "SwapLimit", "CpuCfsQuota", "CpuCfsPeriod"} {
		t.Run(field, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, false)
			r.Config.Resources = testResources(0.5, 512)
			old := m.Run
			m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name == "docker" && args[0] == "info" {
					info := map[string]bool{"MemoryLimit": true, "SwapLimit": true, "CpuCfsQuota": true, "CpuCfsPeriod": true}
					info[field] = false
					return dockerJSON(info)
				}
				return old(ctx, name, args, input)
			}
			err := m.multiEnsureCandidate(context.Background(), j, r)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("want unsupported %s, got %v", field, err)
			}
			if len(f.containers) != 0 {
				t.Fatal("unsupported candidate executed")
			}
		})
	}
}

func TestResourcesCapabilitiesAreRequestedOnly(t *testing.T) {
	cpu, memory := 0.01, int64(6)
	for _, test := range []struct {
		name   string
		limits *config.Resources
		info   string
		want   []string
	}{
		{"cpu", &config.Resources{CPUs: &cpu}, `{"CpuCfsQuota":true,"CpuCfsPeriod":true,"NCPU":4}`, []string{"--cpus", "0.01"}},
		{"memory", &config.Resources{MemoryMB: &memory}, `{"MemoryLimit":true,"SwapLimit":true}`, []string{"--memory", "6291456", "--memory-swap", "6291456"}},
		{"nil", nil, "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, _ := setup(t)
			calls := 0
			m.Run = func(_ context.Context, name string, args []string, _ []byte) (process.Result, error) {
				calls++
				if name != "docker" || !reflect.DeepEqual(args, []string{"info", "--format", "{{json .}}"}) {
					t.Fatalf("unexpected capability call %s %v", name, args)
				}
				return process.Result{Stdout: []byte(test.info)}, nil
			}
			if err := m.requireResourceSupport(context.Background(), test.limits); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resourceArgs(test.limits), test.want) {
				t.Fatal(resourceArgs(test.limits))
			}
			if test.limits == nil && calls != 0 {
				t.Fatal("nil path probes Docker")
			}
		})
	}
	for _, output := range []string{`{}`, `null`, `not JSON`, `{"MemoryLimit":"true"}`} {
		m, _ := setup(t)
		m.Run = func(_ context.Context, _ string, _ []string, _ []byte) (process.Result, error) {
			return process.Result{Stdout: []byte(output)}, nil
		}
		if err := m.requireResourceSupport(context.Background(), testResources(1, 6)); err == nil {
			t.Fatalf("accepted missing/invalid capabilities: %s", output)
		}
	}
	m, _ := setup(t)
	m.Run = func(_ context.Context, _ string, _ []string, _ []byte) (process.Result, error) {
		return process.Result{Stderr: []byte("private daemon details")}, errors.New("private daemon details")
	}
	if err := m.requireResourceSupport(context.Background(), testResources(1, 6)); err == nil || strings.Contains(err.Error(), "private daemon details") {
		t.Fatalf("bad capability error: %v", err)
	}
}

func TestResourcesLegacyRejected(t *testing.T) {
	a := app()
	s := a.Services["web"]
	s.Resources = testResources(0.5, 512)
	a.Services["web"] = s
	if err := validateLegacyApp(a); err == nil {
		t.Fatal("legacy path silently ignores resources")
	}
	_, req := setup(t)
	req.Config = a
	if err := ValidateRequest(req); err == nil || !strings.Contains(err.Error(), "resources") {
		t.Fatalf("legacy request did not fail closed: %v", err)
	}
}

func TestResourcesDeploymentUpdatesAndClears(t *testing.T) {
	for _, volume := range []bool{false, true} {
		t.Run(map[bool]string{false: "stateless", true: "volume"}[volume], func(t *testing.T) {
			f := newMultiFlow(t)
			a := multiApp()
			if volume {
				a = volumeFlowApp("one")
			}
			previous := map[string]string{}
			for i, limits := range []*config.Resources{testResources(0.5, 512), testResources(1.001, 1024), nil} {
				for name, s := range a.Services {
					s.Resources = limits
					a.Services[name] = s
				}
				f.docker.calls = nil
				if _, err := f.deploy(a, strings.Repeat(string(rune('1'+i)), 32)); err != nil {
					t.Fatal(err)
				}
				j := f.load()
				for name, slot := range j.ServiceStates {
					if slot.Current.Container == previous[name] {
						t.Fatal("resource change did not replace container")
					}
					previous[name] = slot.Current.Container
					v := f.docker.containers[slot.Current.Container]
					if v.HostConfig.NanoCPUs != limits.NanoCPUs() || v.HostConfig.Memory != limits.MemoryBytes() || v.HostConfig.MemorySwap != limits.MemoryBytes() {
						t.Fatalf("wrong caps: %+v", v.HostConfig)
					}
					if !reflect.DeepEqual(slot.Current.Config.Resources, limits) {
						t.Fatal("resources not persisted")
					}
				}
				for _, call := range f.docker.calls {
					if strings.HasPrefix(call, "buildx ") && strings.Contains(call, "--memory") {
						t.Fatal("runtime limit applied to build")
					}
					if limits == nil && strings.HasPrefix(call, "info ") {
						t.Fatal("nil resource path probes capabilities")
					}
					if limits == nil && (strings.HasPrefix(call, "run ") || strings.HasPrefix(call, "create ")) && (strings.Contains(call, "--cpus") || strings.Contains(call, "--memory")) {
						t.Fatal("nil resource path sets limits")
					}
				}
			}
		})
	}
}

func TestResourcesRuntimeTamperingRejected(t *testing.T) {
	yes := true
	mutations := map[string]func(*inspection){
		"cpu cleared":    func(v *inspection) { v.HostConfig.NanoCPUs = 0 },
		"cpu changed":    func(v *inspection) { v.HostConfig.NanoCPUs++ },
		"memory cleared": func(v *inspection) { v.HostConfig.Memory = 0 },
		"memory changed": func(v *inspection) { v.HostConfig.Memory++ },
		"swap cleared":   func(v *inspection) { v.HostConfig.MemorySwap = 0 },
		"swap unlimited": func(v *inspection) { v.HostConfig.MemorySwap = -1 },
		"period":         func(v *inspection) { v.HostConfig.CPUPeriod = 100000 },
		"quota":          func(v *inspection) { v.HostConfig.CPUQuota = 50000 },
		"shares":         func(v *inspection) { v.HostConfig.CPUShares = 1024 },
		"cpuset":         func(v *inspection) { v.HostConfig.CpusetCpus = "0" },
		"memset":         func(v *inspection) { v.HostConfig.CpusetMems = "0" },
		"reservation":    func(v *inspection) { v.HostConfig.MemoryReservation = 1024 },
		"oom disabled":   func(v *inspection) { v.HostConfig.OomKillDisable = &yes },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			m, j, r, f := multiDockerFixture(t, false)
			r.Config.Resources = testResources(0.5, 512)
			if err := m.multiEnsureCandidate(context.Background(), j, r); err != nil {
				t.Fatal(err)
			}
			mutate(f.containers[r.Container])
			if _, err := m.multiInspect(context.Background(), j, r, false, true); err == nil {
				t.Fatal("accepted resource drift")
			}
		})
	}
	m, j, r, f := multiDockerFixture(t, false)
	if err := m.multiEnsureCandidate(context.Background(), j, r); err != nil {
		t.Fatal(err)
	}
	f.containers[r.Container].HostConfig.NanoCPUs = 500000000
	if _, err := m.multiInspect(context.Background(), j, r, false, true); err == nil {
		t.Fatal("accepted added limit on unlimited service")
	}
}

func TestResourcesVolumeMismatchBeforeStart(t *testing.T) {
	f := newMultiFlow(t)
	a := volumeFlowApp("one")
	for name, s := range a.Services {
		s.Resources = testResources(0.5, 512)
		a.Services[name] = s
	}
	f.docker.inspectHook = func(v *inspection) { v.HostConfig.MemorySwap = 0 }
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err == nil {
		t.Fatal("accepted ignored memory swap limit")
	}
	if eventCount(f.docker, "start") != 0 {
		t.Fatal("started unsafe volume writer")
	}
}

func TestResourcesScopedAndManualComparison(t *testing.T) {
	f := newMultiFlow(t)
	a := multiApp()
	backend := a.Services["backend"]
	backend.Update = config.UpdateManual
	backend.Resources = testResources(0.5, 512)
	a.Services["backend"] = backend
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load().ServiceStates["backend"].Current
	backend.Resources = testResources(1, 512)
	a.Services["backend"] = backend
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("manual resource edit accepted: %v", err)
	}
	q := f.req
	q.Config, q.Service, q.Operation = a, "web", strings.Repeat("3", 32)
	selected := &config.App{Name: a.Name, Services: map[string]config.Service{"web": a.Services["web"]}}
	if _, err := f.m.Execute(context.Background(), q, f.bundle(selected)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, f.load().ServiceStates["backend"].Current) {
		t.Fatal("scoped deploy changed unselected caps")
	}
	q.Service, q.Operation, q.Apply = "backend", strings.Repeat("4", 32), true
	selected.Services = map[string]config.Service{"backend": backend}
	if _, err := f.m.Execute(context.Background(), q, f.bundle(selected)); err != nil {
		t.Fatal(err)
	}
	if f.load().ServiceStates["backend"].Current.Config.Resources.NanoCPUs() != 1000000000 {
		t.Fatal("explicit manual resource edit not deployed")
	}
}

func TestResourcesRunInheritsAndVerifies(t *testing.T) {
	for _, mode := range []string{"normal", "tampered", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			a := multiApp()
			cfg := a.Services["backend"]
			cfg.Resources = testResources(0.125, 64)
			a.Services["backend"] = cfg
			q := f.req
			q.Action, q.Operation, q.Service, q.Command, q.Config = "run", strings.Repeat("2", 32), "backend", []string{"task"}, a
			state, err := f.m.secretStore().Load(j.App, j.HostID)
			if err != nil {
				t.Fatal(err)
			}
			old := f.m.Run
			starts := 0
			f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name == "docker" && args[0] == "info" && mode == "unsupported" {
					return dockerJSON(map[string]bool{})
				}
				if name == "docker" && args[0] == "start" {
					starts++
					f.docker.containers["vamos-run-"+q.Operation].State.Running = true
					return process.Result{}, errors.New("lost start response")
				}
				return old(ctx, name, args, input)
			}
			if mode == "tampered" {
				f.docker.inspectHook = func(v *inspection) {
					if v.Config.Labels["vamos.run"] != "" {
						v.HostConfig.NanoCPUs = 0
					}
				}
			}
			selected := &config.App{Name: a.Name, Services: map[string]config.Service{"backend": cfg}}
			_, err = f.m.executeRun(context.Background(), q, f.bundle(selected), j, &lifecycle.State{ID: j.HostID}, state)
			if mode != "normal" {
				if err == nil || starts != 0 {
					t.Fatalf("unsafe run: starts=%d err=%v", starts, err)
				}
				return
			}
			if err != nil || starts != 1 {
				t.Fatalf("run: starts=%d err=%v", starts, err)
			}
			rec, err := f.m.loadRun(j, q.Operation)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rec.Config.Resources, cfg.Resources) {
				t.Fatal("run did not capture service limits")
			}
			v := f.docker.containers[rec.Container]
			if v.HostConfig.NanoCPUs != 125000000 || v.HostConfig.Memory != 67108864 || v.HostConfig.MemorySwap != 67108864 {
				t.Fatalf("wrong run caps: %+v", v.HostConfig)
			}
			if err := f.m.verifyRunEndpoint(context.Background(), j, v); err != nil {
				t.Fatal(err)
			}
			v.HostConfig.Memory = 0
			if err := f.m.verifyRunEndpoint(context.Background(), j, v); err == nil {
				t.Fatal("accepted run limit drift")
			}
		})
	}
}

func TestResourcesNilOldRunCanonical(t *testing.T) {
	const old = `{"Version":1,"App":"test","HostID":"host","operation":"op","service":"worker","phase":"building","container":"container","image":"","exit_code":null,"DockerID":"","Config":{"Build":null,"Image":"alpine","Command":null,"Env":null,"Secrets":null,"HTTP":null,"Update":"auto"},"SecretRefs":null}`
	var rec runRecord
	if err := json.Unmarshal([]byte(old), &rec); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rec)
	if err != nil || string(b) != old {
		t.Fatalf("old canonical run changed: %s, %v", b, err)
	}
}
