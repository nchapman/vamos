package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

type releaseFlow struct {
	*multiFlow
	starts        int
	exit          int
	loseStart     bool
	loseCreate    bool
	neverStart    bool
	interruptWait bool
	done          map[string]bool
	create        []string
	onStart       func()
}

func newReleaseFlow(t *testing.T) *releaseFlow {
	f := &releaseFlow{multiFlow: newMultiFlow(t), done: map[string]bool{}}
	f.req.Partial = true
	original := f.m.Run
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "docker" {
			if args[0] == "create" {
				f.create = slices.Clone(args)
				if f.loseCreate {
					result, err := original(ctx, name, args, input)
					if err != nil {
						return result, err
					}
					return process.Result{}, errors.New("lost create response")
				}
			}
			var v *inspection
			for _, candidate := range f.docker.containers {
				if candidate.Config.Labels["vamos.run"] != "" && (args[len(args)-1] == candidate.ID || args[len(args)-1] == strings.TrimPrefix(candidate.Name, "/")) {
					v = candidate
					break
				}
			}
			if v != nil {
				switch args[0] {
				case "start":
					f.starts++
					f.docker.events = append(f.docker.events, "release-start")
					if f.onStart != nil {
						f.onStart()
					}
					if !f.neverStart {
						v.State.Running = true
						v.State.StartedAt = "2026-06-01T00:00:00Z"
					}
					if f.loseStart {
						return process.Result{}, errors.New("lost start response")
					}
					return process.Result{}, nil
				case "wait":
					if f.interruptWait {
						return process.Result{}, errors.New("wait interrupted")
					}
					v.State.Running = false
					f.done[v.ID] = true
					f.docker.events = append(f.docker.events, "release-complete")
					return process.Result{}, nil
				case "container":
					if len(args) == 3 && args[1] == "inspect" && f.done[v.ID] {
						b, _ := json.Marshal(v)
						var obj map[string]any
						_ = json.Unmarshal(b, &obj)
						state := obj["State"].(map[string]any)
						state["Status"], state["ExitCode"], state["FinishedAt"] = "exited", f.exit, "2026-06-01T00:01:00Z"
						return dockerJSON([]any{obj})
					}
				}
			}
		}
		return original(ctx, name, args, input)
	}
	return f
}

func releaseApp(value string) *config.App {
	a := changedMultiApp(value)
	a.Release = &config.Release{Service: "web", Command: []string{"bin/release", "literal $(id)", "space argument"}}
	return a
}
func (f *releaseFlow) bind() {
	j := f.load()
	f.req.HostIdentity = j.HostID
	f.req.Bindings = map[string]string{}
	for name, s := range j.ServiceStates {
		f.req.Bindings[name] = s.Incarnation
	}
}
func (f *releaseFlow) resumeHook(op string) (Response, error) {
	q := f.req
	q.Action = "resume"
	q.Supersede = ""
	q.Operation = op
	q.Config = nil
	q.ContextFormat = ""
	q.Partial = false
	q.Bindings = nil
	return f.m.Execute(context.Background(), q, nil)
}

func TestReleaseCommandGatesEveryRuntimeAndUsesCapturedInputs(t *testing.T) {
	f := newReleaseFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	f.bind()
	stageMultiSecret(t, f.multiFlow, strings.Repeat("b", 32), "release-pinned-secret")
	a := releaseApp("new")
	cfg := a.Services["web"]
	cfg.Secrets = map[string]string{"TOKEN": "credential"}
	a.Services["web"] = cfg
	f.onStart = func() {
		for _, s := range old.ServiceStates {
			if !f.docker.containers[s.Current.Container].State.Running {
				t.Fatal("runtime retired before release completed")
			}
		}
	}
	f.docker.events = nil
	resp, err := f.deploy(a, strings.Repeat("2", 32))
	if err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if f.starts != 1 || j.Version != 8 || j.Hook == nil || j.Hook.ExitCode == nil || *j.Hook.ExitCode != 0 || resp.Status.Phase != "ready" {
		t.Fatalf("starts=%d journal=%+v", f.starts, j)
	}
	if j.Config.Release != nil || a.Release == nil {
		t.Fatal("directive leaked into desired runtime or mutated caller")
	}
	if slices.Index(f.docker.events, "release-complete") > slices.Index(f.docker.events, "stop") {
		t.Fatal(f.docker.events)
	}
	if !reflect.DeepEqual(f.create[len(f.create)-3:], a.Release.Command) {
		t.Fatal(f.create)
	}
	for _, forbidden := range []string{"--publish", "--mount", "--network-alias", "--entrypoint", "--rm"} {
		if slices.Contains(f.create, forbidden) {
			t.Fatal(f.create)
		}
	}
	hook := f.docker.containers["vamos-run-"+j.Hook.Operation]
	if !strings.Contains(strings.Join(hook.Config.Env, "\n"), "TOKEN=release-pinned-secret") || !strings.Contains(strings.Join(hook.Config.Env, "\n"), "REVISION=new") {
		t.Fatal(hook.Config.Env)
	}
	if resp.Status.Release == nil || resp.Status.Release.ParentOperation != j.Operation || resp.Status.Release.Output != "" {
		t.Fatal(resp.Status.Release)
	}
	if _, err = f.resumeHook(j.Operation); err != nil || f.starts != 1 {
		t.Fatalf("resume restarted: %v starts=%d", err, f.starts)
	}
	f.bind()
	if _, err = f.deploy(changedMultiApp("no-hook"), strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	if f.load().Version != 8 || f.load().Hook != nil || f.starts != 1 {
		t.Fatal("hook inherited or journal downgraded")
	}
}

func TestReleaseCommandFailureBlocksRolloutAndNeverRetries(t *testing.T) {
	f := newReleaseFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	f.bind()
	f.exit = 7
	op := strings.Repeat("2", 32)
	if _, err := f.deploy(releaseApp("failed"), op); err == nil || !strings.Contains(err.Error(), "exit code 7") {
		t.Fatal(err)
	}
	j := f.load()
	if j.Phase != "releasing" || *j.Hook.ExitCode != 7 {
		t.Fatal(j)
	}
	for name, s := range old.ServiceStates {
		if !reflect.DeepEqual(s.Current, j.ServiceStates[name].Current) || !f.docker.containers[s.Current.Container].State.Running {
			t.Fatal("failed release changed runtime")
		}
	}
	if _, err := f.resumeHook(op); err == nil || f.starts != 1 {
		t.Fatalf("failed hook restarted: %v starts=%d", err, f.starts)
	}
	q := f.req
	q.Action = "rollback"
	q.Operation = strings.Repeat("3", 32)
	q.Config = nil
	q.Partial = false
	q.Bindings = nil
	q.ContextFormat = ""
	q.Apply = true
	if _, err := f.m.Execute(context.Background(), q, nil); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.load().Hook != nil {
		t.Fatal("rollback executed hook")
	}
}

func TestReleaseCommandLostStartAndInterruptedWaitResumeObservation(t *testing.T) {
	for _, neverStarted := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "unknown"}[neverStarted], func(t *testing.T) {
			f := newReleaseFlow(t)
			f.loseStart = true
			f.neverStart = neverStarted
			f.interruptWait = true
			op := strings.Repeat("1", 32)
			if _, err := f.deploy(releaseApp("first"), op); err == nil {
				t.Fatal("accepted uncertain execution")
			}
			j := f.load()
			for _, s := range j.ServiceStates {
				if s.Current != nil || s.Candidate != nil {
					t.Fatal("runtime created before release completion")
				}
			}
			f.bind()
			f.req.Supersede = op
			if _, err := f.deploy(releaseApp("second"), strings.Repeat("2", 32)); err == nil {
				t.Fatal("uncertain execution allowed supersession")
			}
			f.interruptWait = false
			_, err := f.resumeHook(op)
			if neverStarted && err == nil {
				t.Fatal("unknown execution allowed rollout")
			}
			if !neverStarted && err != nil {
				t.Fatal(err)
			}
			if f.starts != 1 {
				t.Fatal("start retried", f.starts)
			}
		})
	}
}

func TestLegacyJournalRejectsReleaseMetadata(t *testing.T) {
	for _, field := range []string{"capture", "status"} {
		t.Run(field, func(t *testing.T) {
			m, _ := setup(t)
			j := saveRouteLegacy(t, m, "notes", "building")
			if field == "capture" {
				j.Hook = &releaseCapture{Operation: strings.Repeat("1", 32), Directive: config.Release{Service: "web", Command: []string{"bin/release"}}}
			} else {
				j.Status.Release = &RunResult{Operation: strings.Repeat("1", 32), Service: "web", Phase: "complete"}
			}
			if err := validateJournal(j, j.App); err == nil {
				t.Fatal("legacy journal silently accepted release metadata")
			}
		})
	}
}

func TestReleaseCommandIsNotRepeatedAfterRuntimeReadinessFailure(t *testing.T) {
	f := newReleaseFlow(t)
	f.failHealth["web"] = true
	op := strings.Repeat("1", 32)
	if _, err := f.deploy(releaseApp("first"), op); err == nil {
		t.Fatal("expected runtime readiness failure")
	}
	j := f.load()
	if j.Phase != "updating" || j.Hook.ExitCode == nil || *j.Hook.ExitCode != 0 {
		t.Fatal("successful release gate was not durable")
	}
	f.failHealth["web"] = false
	if _, err := f.resumeHook(op); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.load().Phase != "ready" {
		t.Fatal("runtime recovery repeated the release command")
	}
}

func TestReleaseCommandReplicaGroupSharesLimitsAndOneExecution(t *testing.T) {
	f := newReleaseFlow(t)
	a := releaseApp("group")
	cfg := a.Services["web"]
	count := 2
	cpu := 0.5
	memory := int64(64)
	cfg.Replicas = &count
	cfg.Resources = &config.Resources{CPUs: &cpu, MemoryMB: &memory}
	a.Services["web"] = cfg
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	members := runtimeRefs(j.ServiceStates["web"].Current)
	if len(members) != 2 || f.starts != 1 {
		t.Fatal("release was not once for the whole group")
	}
	names := []string{"vamos-run-" + j.Hook.Operation}
	for _, member := range members {
		names = append(names, member.Container)
	}
	for _, name := range names {
		v := f.docker.containers[name]
		if v == nil || v.HostConfig.NanoCPUs != 500000000 || v.HostConfig.Memory != 64*1024*1024 || v.HostConfig.MemorySwap != v.HostConfig.Memory {
			t.Fatalf("limits differ for %s", name)
		}
	}
}

func TestReleaseCommandRejectsExcludedRunner(t *testing.T) {
	f := newReleaseFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.bind()
	f.req.Service = "backend"
	delete(f.req.Bindings, "web")
	if _, err := f.deploy(releaseApp("excluded"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("accepted excluded runner")
	}
	if f.starts != 0 {
		t.Fatal("release started")
	}
}

func TestReleaseCommandRejectsSkippedManualRunner(t *testing.T) {
	f := newReleaseFlow(t)
	a := multiApp()
	cfg := a.Services["web"]
	cfg.Update = config.UpdateManual
	a.Services["web"] = cfg
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.bind()
	a.Release = &config.Release{Service: "web", Command: []string{"bin/release"}}
	builds := f.builds
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil || !strings.Contains(err.Error(), "non-skipped") {
		t.Fatal(err)
	}
	if f.starts != 0 || f.builds != builds {
		t.Fatal("skipped runner began execution")
	}
}

func TestReleaseCommandRecoversLostCreateResponse(t *testing.T) {
	f := newReleaseFlow(t)
	f.loseCreate = true
	op := strings.Repeat("1", 32)
	if _, err := f.deploy(releaseApp("first"), op); err == nil {
		t.Fatal("expected lost create response")
	}
	if f.starts != 0 {
		t.Fatal("started after uncertain create")
	}
	f.loseCreate = false
	if _, err := f.resumeHook(op); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.load().Phase != "ready" {
		t.Fatal("prestart container not recovered exactly once")
	}
}

func TestReleaseCommandRecoversDurableResultBeforeParentGate(t *testing.T) {
	f := newReleaseFlow(t)
	f.interruptWait = true
	op := strings.Repeat("1", 32)
	if _, err := f.deploy(releaseApp("first"), op); err == nil {
		t.Fatal("expected interrupted wait")
	}
	j := f.load()
	rec, err := f.m.loadRun(j, j.Hook.Operation)
	if err != nil {
		t.Fatal(err)
	}
	v := f.docker.containers[rec.Container]
	v.State.Running = false
	f.done[v.ID] = true
	rec.RunResult = *f.m.observeRun(context.Background(), j, rec)
	if err = f.m.saveRun(j, rec); err != nil {
		t.Fatal(err)
	}
	if j.Hook.ExitCode != nil {
		t.Fatal("parent gate unexpectedly recorded")
	}
	if _, err = f.resumeHook(op); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.load().Phase != "ready" {
		t.Fatal("durable result not reconciled")
	}
}
