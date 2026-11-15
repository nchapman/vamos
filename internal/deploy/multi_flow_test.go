package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

type multiFlow struct {
	t          *testing.T
	m          *Manager
	req        Request
	docker     *fakeDocker
	live       []byte
	failHealth map[string]bool
	failBuild  int
	builds     int
}

func multiApp() *config.App {
	a := app()
	a.Services["backend"] = config.Service{Build: &config.Build{Context: ".", Dockerfile: "Backendfile"}, Update: config.UpdateAuto}
	return a
}

func newMultiFlow(t *testing.T) *multiFlow {
	t.Helper()
	m, r := setup(t)
	r.Config = multiApp()
	r.ContextFormat = BundleFormat
	m.PrivateObserve = 0
	nextPort := 20000
	m.AllocatePort = func() (string, error) { nextPort++; return fmt.Sprint(nextPort), nil }
	if err := atomic(m.path("/etc/docker/daemon.json"), []byte(`{"bridge":"none","log-driver":"local"}`), 0644); err != nil {
		t.Fatal(err)
	}
	base := &journal{HostID: strings.Repeat("a", 32), Status: Status{App: r.App}}
	f := &multiFlow{t: t, m: m, req: r, failHealth: map[string]bool{}}
	f.live, _ = json.Marshal(map[string]string{"text": baseCaddy})
	f.docker = newFakeDocker(t, m, base)
	f.docker.buildHook = func(_ []string, input []byte) string {
		sum := sha256.Sum256(input)
		return fmt.Sprintf("sha256:%x", sum)
	}
	m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if ca, ok := caddyCall(m, name, args); ok {
			name, args = "caddy", ca
		}
		if name == "caddy" {
			switch args[0] {
			case "adapt":
				b, err := os.ReadFile(args[2])
				if err != nil {
					return process.Result{}, err
				}
				return dockerJSON(map[string]string{"text": string(b)})
			case "validate":
				return process.Result{}, nil
			case "reload":
				b, err := os.ReadFile(args[2])
				if err != nil {
					return process.Result{}, err
				}
				f.live, _ = json.Marshal(map[string]string{"text": string(b)})
				f.docker.events = append(f.docker.events, "reload")
				return process.Result{}, nil
			}
		}
		if name == "docker" && args[0] == "buildx" {
			f.builds++
			if f.failBuild == f.builds {
				return process.Result{}, errors.New("test build failed")
			}
		}
		if name == "docker" && args[0] == "logs" {
			return process.Result{Stdout: []byte(args[len(args)-1] + " logs")}, nil
		}
		return f.docker.run(ctx, name, args, input)
	}
	m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "127.0.0.1:2019" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(f.live)))}, nil
		}
		status := 503
		for _, v := range f.docker.containers {
			for _, bindings := range v.HostConfig.PortBindings {
				for _, binding := range bindings {
					if req.URL.Host == "127.0.0.1:"+binding.HostPort && v.State.Running && len(v.NetworkSettings.Networks) > 0 {
						name := v.Config.Labels["vamos.service"]
						if name == "" {
							name = "web"
						}
						if !f.failHealth[name] {
							status = 200
						}
					}
				}
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("test health"))}, nil
	})
	return f
}

func (f *multiFlow) bundle(a *config.App) []byte {
	f.t.Helper()
	b, err := buildcontext.Bundle("vamos.yaml", a, func(_ string, s config.Build) ([]byte, error) { return []byte("captured:" + s.Dockerfile), nil })
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}
func (f *multiFlow) deploy(a *config.App, op string) (Response, error) {
	q := f.req
	q.Config = a
	q.Operation = op
	return f.m.Execute(context.Background(), q, f.bundle(a))
}
func (f *multiFlow) load() *journal {
	f.t.Helper()
	j, err := f.m.load(f.req.App)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}
func (f *multiFlow) resume(op string) (Response, error) {
	q := f.req
	q.Action = "resume"
	q.Config = nil
	q.ContextFormat = ""
	q.Operation = op
	return f.m.Execute(context.Background(), q, nil)
}
func changedMultiApp(value string) *config.App {
	a := multiApp()
	for name, s := range a.Services {
		s.Env = map[string]string{"REVISION": value}
		a.Services[name] = s
	}
	return a
}

func TestMultiFlowBuildsThenCreatesPrivateBeforePublic(t *testing.T) {
	f := newMultiFlow(t)
	resp, err := f.deploy(multiApp(), strings.Repeat("1", 32))
	if err != nil {
		t.Fatal(resp, err)
	}
	j := f.load()
	if j.Version != 2 || j.Phase != "ready" || j.Completed == nil || len(j.Completed.Services) != 2 || !resp.Status.Running {
		t.Fatalf("not ready: %#v", resp.Status)
	}
	if len(resp.Status.Services) != 2 || resp.Status.Services["backend"].Kind != "private" || resp.Status.Services["backend"].Port != "" {
		t.Fatal(resp.Status)
	}
	if f.builds != 2 || len(f.docker.containers) != 2 {
		t.Fatal("wrong build/container count")
	}
	firstRun := -1
	lastBuild := -1
	for i, event := range f.docker.events {
		if event == "build" {
			lastBuild = i
		}
		if event == "run" && firstRun < 0 {
			firstRun = i
		}
	}
	if firstRun <= lastBuild {
		t.Fatal("runtime mutated before all builds", f.docker.events)
	}
	firstName := ""
	for _, call := range f.docker.calls {
		if strings.HasPrefix(call, "run ") {
			firstName = call
			break
		}
	}
	if !strings.Contains(firstName, "--network-alias backend") || strings.Contains(firstName, "--publish") {
		t.Fatal(firstName)
	}
	if !strings.Contains(string(f.live), "notes.example.com") || strings.Contains(string(f.live), "notes-backend") {
		t.Fatal(string(f.live))
	}
}

func TestMultiFlowBuildFailureLeavesExistingServicesUntouched(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load()
	events := len(f.docker.events)
	live := string(f.live)
	f.failBuild = f.builds + 2
	_, err := f.deploy(changedMultiApp("two"), strings.Repeat("2", 32))
	if err == nil {
		t.Fatal("build failure accepted")
	}
	after := f.load()
	if after.Phase != "building" || after.Completed.Operation != before.Completed.Operation || string(f.live) != live {
		t.Fatal("effective state changed")
	}
	for _, event := range f.docker.events[events:] {
		if event != "build" {
			t.Fatal("runtime mutation during failed build", event)
		}
	}
	for _, s := range after.ServiceStates {
		if !f.docker.containers[s.Current.Container].State.Running {
			t.Fatal("old stopped")
		}
	}
	f.failBuild = 0
	if _, err = f.resume(after.Operation); err != nil {
		t.Fatal(err)
	}
	if f.load().Phase != "ready" {
		t.Fatal("resume not ready")
	}
}

func TestMultiFlowPrivateFailurePreservesPromotedPublicAndPinsResume(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	backend := old.ServiceStates["backend"].Current.Container
	op := strings.Repeat("2", 32)
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			if f.docker.containers[backend].State.Running || f.docker.containers[backend].HostConfig.RestartPolicy.Name != "no" {
				t.Fatal("private overlap")
			}
			f.docker.lose["run"] = 1
		}
	}
	resp, err := f.deploy(changedMultiApp("two"), op)
	if err == nil {
		t.Fatal("lost create response ignored")
	}
	pending := f.load()
	if pending.Phase != "updating" || pending.ServiceStates["web"].Current.Operation != op || pending.ServiceStates["backend"].Current.Operation == op || !pending.ServiceStates["backend"].CurrentRetired || pending.Completed.Operation != old.Completed.Operation {
		t.Fatal("mixed state lost")
	}
	if resp.Status.Services["backend"].Available {
		t.Fatal("retired private service claimed available")
	}
	runs := eventCount(f.docker, "run")
	f.docker.beforeRun = nil
	if _, err = f.resume(op); err != nil {
		t.Fatal(err)
	}
	ready := f.load()
	if ready.Completed.Operation != op || ready.PreviousCompleted.Operation != old.Completed.Operation || eventCount(f.docker, "run") != runs {
		t.Fatal("resume rebuilt/recreated completed work")
	}
	if f.docker.containers[backend].State.Running || f.docker.containers[backend].HostConfig.RestartPolicy.Name != "no" {
		t.Fatal("retired worker resurrected")
	}
}

func TestMultiFlowSupersedeKeepsCompletedProgressAndSnapshots(t *testing.T) {
	f := newMultiFlow(t)
	first := strings.Repeat("1", 32)
	if _, err := f.deploy(multiApp(), first); err != nil {
		t.Fatal(err)
	}
	second := strings.Repeat("2", 32)
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			f.docker.lose["run"] = 1
		}
	}
	if _, err := f.deploy(changedMultiApp("two"), second); err == nil {
		t.Fatal("expected stopped operation")
	}
	partial := f.load()
	promoted := partial.ServiceStates["web"].Current.Container
	f.docker.beforeRun = nil
	q := f.req
	q.Config = changedMultiApp("three")
	q.Operation = strings.Repeat("3", 32)
	q.Supersede = second
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err != nil {
		t.Fatal(err)
	}
	final := f.load()
	if final.PreviousCompleted.Operation != first || final.Completed.Operation != q.Operation {
		t.Fatal("partial operation became completed snapshot")
	}
	if f.docker.containers[promoted].State.Running {
		t.Fatal("superseded intermediate web still running")
	}
	if !retainedContainer(final, final.PreviousCompleted.Services["web"].Container) {
		t.Fatal("previous successful web lost")
	}
}

func TestMultiFlowDoesNotCompleteWithStoppedEffectiveService(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	op := strings.Repeat("2", 32)
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			v := f.docker.containers[containerName(f.req.App, "web", op)]
			v.State.Running = false
			v.NetworkSettings.Ports = nil
		}
	}
	if _, err := f.deploy(changedMultiApp("two"), op); err == nil {
		t.Fatal("completed snapshot claimed a stopped public service")
	}
	j := f.load()
	if j.Phase != "updating" || j.Completed.Operation == op {
		t.Fatal("failed availability check advanced completed history")
	}
}

func TestMultiFlowNoopDoesNotRetirePreviousCompletedSnapshot(t *testing.T) {
	f := newMultiFlow(t)
	for _, op := range []string{"1", "2"} {
		if _, err := f.deploy(changedMultiApp(op), strings.Repeat(op, 32)); err != nil {
			t.Fatal(err)
		}
	}
	runs := eventCount(f.docker, "run")
	if _, err := f.deploy(changedMultiApp("2"), strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if eventCount(f.docker, "run") != runs || j.Completed.Operation != strings.Repeat("2", 32) || j.PreviousCompleted.Operation != strings.Repeat("1", 32) {
		t.Fatal("noop changed effective release history")
	}
}

func TestMultiFlowRemovalAndKindChangesFailBeforeBuild(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.builds
	for _, a := range []*config.App{app(), func() *config.App {
		a := multiApp()
		s := a.Services["backend"]
		s.HTTP = &config.HTTP{Port: 8080, Health: "/ready"}
		a.Services["backend"] = s
		return a
	}()} {
		if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil {
			t.Fatal("unsupported transition accepted")
		}
	}
	if f.builds != before || f.load().Operation != strings.Repeat("1", 32) {
		t.Fatal("preflight mutated app")
	}
}

func TestMultiFlowCapturedBundleCorruptionBlocksResume(t *testing.T) {
	f := newMultiFlow(t)
	f.failBuild = 1
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err == nil {
		t.Fatal("expected failure")
	}
	j := f.load()
	path := filepath.Join(f.m.dir(j.App), j.Archive)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	before := f.builds
	if _, err = f.resume(j.Operation); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal(err)
	}
	if f.builds != before {
		t.Fatal("corrupt captured input used")
	}
}

func TestCurrentClientCanSupersedeFailedLegacyWeb(t *testing.T) {
	f := newMultiFlow(t)
	q := f.req
	q.Config = app()
	q.ContextFormat = ""
	q.Operation = strings.Repeat("1", 32)
	f.failHealth["web"] = true
	if _, err := f.m.Execute(context.Background(), q, []byte("captured:Dockerfile")); err == nil {
		t.Fatal("expected legacy readiness failure")
	}
	f.failHealth["web"] = false
	q.ContextFormat = BundleFormat
	q.Supersede = q.Operation
	q.Operation = strings.Repeat("2", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err != nil {
		t.Fatal("current client cannot finish legacy recovery", err)
	}
	j := f.load()
	if j.Version != 1 || j.Phase != "ready" {
		t.Fatal("legacy recovery unexpectedly migrated state")
	}
	if _, err := f.deploy(app(), strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	if f.load().Version != 2 {
		t.Fatal("ready legacy deployment did not upgrade")
	}
}

func TestMultiFlowLegacyRedeployAddsServiceDNS(t *testing.T) {
	f := newMultiFlow(t)
	q := f.req
	q.Config = app()
	q.ContextFormat = ""
	q.Operation = strings.Repeat("1", 32)
	if _, err := f.m.Execute(context.Background(), q, []byte("captured:Dockerfile")); err != nil {
		t.Fatal(err)
	}
	legacy := f.load()
	old := legacy.Current.Container
	if _, err := f.deploy(app(), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.ServiceStates["web"].Current.Service != "web" || j.ServiceStates["web"].Current.Container == old {
		t.Fatal("successful migration left legacy service without DNS")
	}
	if f.docker.containers[old].State.Running {
		t.Fatal("legacy previous container not retired")
	}
}
