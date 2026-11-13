package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func recoveryRollback(f *multiFlow, op, service string, apply, downtime bool) (Response, error) {
	q := f.req
	q.Action, q.Operation, q.Service = "rollback", op, service
	q.Config, q.ContextFormat = nil, ""
	q.Apply, q.AllowDowntime = apply, downtime
	return f.m.Execute(context.Background(), q, nil)
}

// Rollback and resume must consume captured inputs, not rebuild from a source archive.
func recoveryForbidBuilds(t *testing.T, f *multiFlow) {
	t.Helper()
	run := f.m.Run
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "docker" && len(args) > 0 && (args[0] == "buildx" || args[0] == "build" || args[0] == "pull") {
			t.Fatalf("rollback attempted source/image acquisition: %v", args)
		}
		return run(ctx, name, args, input)
	}
}

func TestRollbackRecoveryPartialDeploymentTargetsLastCompleted(t *testing.T) {
	f := newMultiFlow(t)
	first, partial, rollback := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	if _, err := f.deploy(changedMultiApp("one"), first); err != nil {
		t.Fatal(err)
	}
	target := f.load().Completed
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			f.docker.lose["run"] = 1
		}
	}
	if _, err := f.deploy(changedMultiApp("two"), partial); err == nil {
		t.Fatal("expected partial deployment")
	}
	f.docker.beforeRun = nil
	interrupted := f.load()
	if interrupted.Completed.Operation != first || interrupted.ServiceStates["web"].Current.Config.Env["REVISION"] != "two" {
		t.Fatal("fixture did not advance public service while retaining completed history")
	}
	recoveryForbidBuilds(t, f)
	preview, err := recoveryRollback(f, rollback, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RollbackPreview == nil || preview.RollbackPreview.TargetOperation != first {
		t.Fatalf("wrong partial target: %+v", preview.RollbackPreview)
	}
	if f.load().Operation != partial {
		t.Fatal("preview replaced interrupted operation")
	}
	f.failHealth["web"] = false
	if _, err := recoveryRollback(f, rollback, "", true, false); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Phase != "ready" {
		t.Fatalf("rollback not ready: %s", j.Phase)
	}
	for name, want := range target.Services {
		got := j.ServiceStates[name].Current
		if got.Image != want.Image || got.Config.Env["REVISION"] != "one" {
			t.Fatalf("%s did not restore captured image/config: %+v", name, got)
		}
	}
}

func TestRollbackRecoveryReadinessFailureKeepsPublicServingAndResumes(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(changedMultiApp("one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deploy(changedMultiApp("two"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load().ServiceStates["web"].Current
	routes := append([]byte(nil), f.live...)
	recoveryForbidBuilds(t, f)
	f.failHealth["web"] = true
	op := strings.Repeat("3", 32)
	if _, err := recoveryRollback(f, op, "web", true, false); err == nil {
		t.Fatal("rollback accepted failed readiness")
	}
	if !bytes.Equal(routes, f.live) {
		t.Fatal("failed candidate changed public routes")
	}
	runtime := f.docker.containers[old.Container]
	if runtime == nil || !runtime.State.Running || len(runtime.NetworkSettings.Networks) == 0 {
		t.Fatal("old public runtime no longer serving")
	}
	j := f.load()
	if j.Operation != op || j.Completed.Operation != strings.Repeat("2", 32) {
		t.Fatal("failure lost resumable rollback or advanced history")
	}
	f.failHealth["web"] = false
	if _, err := f.resume(op); err != nil {
		t.Fatal(err)
	}
	j = f.load()
	if j.Phase != "ready" || j.ServiceStates["web"].Current.Config.Env["REVISION"] != "one" {
		t.Fatal("resume did not finish captured rollback")
	}
	if j.ServiceStates["backend"].Current.Config.Env["REVISION"] != "two" {
		t.Fatal("scoped rollback changed excluded private service")
	}
}

func TestRollbackRecoveryVolumeRequiresScopedDowntimeAndResumesSingleWriter(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deploy(volumeFlowApp("two"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load()
	old := before.ServiceStates["web"].Current
	op := strings.Repeat("3", 32)
	recoveryForbidBuilds(t, f)
	if _, err := recoveryRollback(f, op, "", false, true); err == nil {
		t.Fatal("unscoped downtime accepted")
	}
	preview, err := recoveryRollback(f, op, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range preview.RollbackPreview.Services {
		if s.ExcludedReason == "" {
			t.Fatal("unscoped downtime selected a volume writer")
		}
	}
	if _, err := recoveryRollback(f, op, "web", true, false); err == nil {
		t.Fatal("volume rollback accepted without downtime consent")
	}
	if f.load().Operation != before.Operation {
		t.Fatal("rejected rollback mutated journal")
	}
	// Interrupt after the old writer is retired but before the replacement is ready.
	f.failHealth["web"] = true
	if _, err := recoveryRollback(f, op, "web", true, true); err == nil {
		t.Fatal("expected readiness interruption")
	}
	if v := f.docker.containers[old.Container]; v != nil && (v.State.Running || v.HostConfig.RestartPolicy.Name != "no") {
		t.Fatal("old volume writer remained active/restartable")
	}
	assertSingleWriter := func() {
		t.Helper()
		running := 0
		for _, v := range f.docker.containers {
			if v.Config.Labels["soar.service"] == "web" && v.State.Running {
				running++
			}
		}
		if running > 1 {
			t.Fatalf("%d concurrent volume writers", running)
		}
	}
	assertSingleWriter()
	f.failHealth["web"] = false
	if _, err := f.resume(op); err != nil {
		t.Fatal(err)
	}
	assertSingleWriter()
	after := f.load()
	if after.Phase != "ready" || after.ServiceStates["web"].VolumeID != before.ServiceStates["web"].VolumeID || after.ServiceStates["web"].Current.Config.Env["REVISION"] != "one" {
		t.Fatal("resume failed to restore release on same volume")
	}
	if after.ServiceStates["backend"].Current.Container != before.ServiceStates["backend"].Current.Container {
		t.Fatal("excluded volume service was replaced")
	}
}
