package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func rollbackRequest(f *multiFlow, op, service string, apply bool) Request {
	r := f.req
	r.Action = "rollback"
	r.Operation = strings.Repeat(op, 32)
	r.Config = nil
	r.ContextFormat = ""
	r.Service = service
	r.Apply = apply
	return r
}
func rollbackApp(version string) *config.App {
	a := changedMultiApp(version)
	s := a.Services["web"]
	s.Build = &config.Build{Context: ".", Dockerfile: "Web" + version}
	a.Services["web"] = s
	return a
}
func TestRollbackPreviewAndApplyCapturedReleaseWithoutSources(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(rollbackApp("1"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	original := f.load().Completed
	if _, err := f.deploy(rollbackApp("2"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	latest := f.load().Completed
	before := workflowFiles(t, f.m.dir(f.req.App))
	events := len(f.docker.events)
	r := rollbackRequest(f, "3", "", false)
	resp, err := f.m.Execute(context.Background(), r, nil)
	if err != nil || resp.RollbackPreview == nil || !resp.RollbackPreview.NeedsConfirmation || resp.RollbackPreview.TargetOperation != original.Operation {
		t.Fatalf("preview %+v: %v", resp, err)
	}
	if !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(f.req.App))) || events != len(f.docker.events) {
		t.Fatal("preview mutated app")
	}
	files, _ := filepath.Glob(filepath.Join(f.m.dir(f.req.App), "*.context.tar"))
	for _, p := range files {
		if err = os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	builds := f.builds
	f.failBuild = builds + 1
	r.Apply = true
	if _, err = f.m.Execute(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 5 || j.Phase != "ready" || j.Archive != "" || f.builds != builds {
		t.Fatal("rollback rebuilt or failed", j.Phase, f.builds)
	}
	for name, wanted := range original.Services {
		got := j.ServiceStates[name].Current
		if got.Image != wanted.Image || !reflect.DeepEqual(got.Config, wanted.Config) || !reflect.DeepEqual(got.SecretRefs, wanted.SecretRefs) {
			t.Fatalf("%s did not restore captured release", name)
		}
	}
	if _, err = f.m.Execute(context.Background(), rollbackRequest(f, "4", "", true), nil); err != nil {
		t.Fatal(err)
	}
	for name, wanted := range latest.Services {
		if f.load().ServiceStates[name].Current.Image != wanted.Image || !reflect.DeepEqual(f.load().ServiceStates[name].Current.Config, wanted.Config) {
			t.Fatal("repeated rollback did not restore previous completed state")
		}
	}
	f.failBuild = 0
	if _, err = f.deploy(rollbackApp("3"), strings.Repeat("5", 32)); err != nil {
		t.Fatal("deploy after rollback", err)
	}
	if f.load().Version != 4 {
		t.Fatal("normal deploy retained rollback-only schema")
	}
}
func TestRollbackKeepsDistinctImagesForSharedSourceKey(t *testing.T) {
	f := newMultiFlow(t)
	a := multiApp()
	s := a.Services["backend"]
	s.Build = a.Services["web"].Build
	a.Services["backend"] = s
	n := 0
	f.docker.buildHook = func(_ []string, _ []byte) string { n++; return fmt.Sprintf("sha256:%064x", n) }
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	backend := f.load().ServiceStates["backend"].Current.Image
	q := f.req
	q.Config = a
	q.Service = "web"
	q.Operation = strings.Repeat("2", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(a)); err != nil {
		t.Fatal(err)
	}
	web := f.load().ServiceStates["web"].Current.Image
	q.Service = "backend"
	q.Operation = strings.Repeat("3", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(a)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "4", "", true), nil); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if web == backend || j.ServiceStates["web"].Current.Image != web || j.ServiceStates["backend"].Current.Image != backend || n != 3 {
		t.Fatal("rollback collapsed distinct captured images or rebuilt")
	}
}
func TestRollbackUsesCapturedSecretsNotStagedVersions(t *testing.T) {
	f := newMultiFlow(t)
	a := multiApp()
	s := a.Services["web"]
	s.Secrets = map[string]string{"TOKEN": "credential"}
	a.Services["web"] = s
	stageMultiSecret(t, f, strings.Repeat("a", 32), "first")
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	first := f.load().ServiceStates["web"].Current.SecretRefs
	stageMultiSecret(t, f, strings.Repeat("b", 32), "second")
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	backend := f.load().ServiceStates["backend"].Current
	stageMultiSecret(t, f, strings.Repeat("c", 32), "third")
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "web", true), nil); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if !reflect.DeepEqual(j.ServiceStates["web"].Current.SecretRefs, first) || !reflect.DeepEqual(j.ServiceStates["backend"].Current, backend) {
		t.Fatal("rollback used latest secrets or changed unselected runtime")
	}
}
func TestRollbackMissingImageFailsBeforeCaptureOrRetirement(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(rollbackApp("1"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	first := f.load().ServiceStates["web"].Current.Image
	if _, err := f.deploy(rollbackApp("2"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	delete(f.docker.images, first)
	before := workflowFiles(t, f.m.dir(f.req.App))
	events := len(f.docker.events)
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "", true), nil); err == nil || !strings.Contains(err.Error(), "image unavailable") {
		t.Fatal("missing immutable input accepted", err)
	}
	if !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(f.req.App))) || events != len(f.docker.events) {
		t.Fatal("failed preflight changed app")
	}
}
func TestRollbackLeavesManualDatabaseUntouched(t *testing.T) {
	f := newWorkflowFlow(t)
	f.bootstrap()
	if _, err := f.selected(workflowApp(false), "", "2", false); err != nil {
		t.Fatal(err)
	}
	a := workflowApp(false)
	web := a.Services["web"]
	web.Env = map[string]string{"REVISION": "2"}
	a.Services["web"] = web
	if _, err := f.selected(a, "web", "3", false); err != nil {
		t.Fatal(err)
	}
	before := f.load().ServiceStates["db"].Current
	pulls := f.pulls
	resp, err := f.m.Execute(context.Background(), rollbackRequest(f.multiFlow, "4", "", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	excluded := false
	for _, s := range resp.RollbackPreview.Services {
		if s.Name == "db" && strings.Contains(s.ExcludedReason, "manual") {
			excluded = true
		}
	}
	if !excluded || !reflect.DeepEqual(f.load().ServiceStates["db"].Current, before) || f.pulls != pulls {
		t.Fatal("manual database was not excluded")
	}
	q := rollbackRequest(f.multiFlow, "5", "db", true)
	q.AllowDowntime = true
	if _, err = f.m.Execute(context.Background(), q, nil); err == nil || !strings.Contains(err.Error(), "manual") {
		t.Fatal("manual override accepted", err)
	}
}
