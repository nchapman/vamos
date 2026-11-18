package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

// workflowFlow extends only the Docker CLI boundary: tags resolve to immutable
// images, while the existing runtime fixture still enforces mount ownership.
type workflowFlow struct {
	*multiFlow
	tag         string
	pulls       int
	failInspect bool
}

func newWorkflowFlow(t *testing.T) *workflowFlow {
	f := &workflowFlow{multiFlow: newMultiFlow(t), tag: "sha256:" + strings.Repeat("a", 64)}
	base := f.m.Run
	f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 0 {
			if a[0] == "pull" {
				if len(a) != 3 || a[1] != "--quiet" || a[2] != "postgres:17" {
					t.Fatalf("unexpected pull: %q", a)
				}
				f.pulls++
				im := &inspection{ID: f.tag}
				im.Config.Volumes = map[string]json.RawMessage{"/var/lib/postgresql/data": json.RawMessage(`{}`)}
				f.docker.images[f.tag] = im
				return process.Result{Stdout: []byte(f.tag + "\n")}, nil
			}
			if len(a) == 3 && a[0] == "image" && a[1] == "inspect" {
				if a[2] == "postgres:17" {
					return dockerJSON([]*inspection{f.docker.images[f.tag]})
				}
				if f.failInspect && a[2] == f.tag {
					return process.Result{}, errors.New("injected immutable inspection outage")
				}
			}
		}
		return base(ctx, n, a, b)
	}
	return f
}
func workflowApp(missing bool) *config.App {
	a := app()
	a.Services["db"] = config.Service{Image: "postgres:17", Update: config.UpdateManual, Volume: &config.Volume{Name: "postgres-data", Mount: "/var/lib/postgresql/data"}}
	if missing {
		s := a.Services["web"]
		s.Build = &config.Build{Context: "missing-directory", Dockerfile: "Missingfile"}
		s.Secrets = map[string]string{"TOKEN": "not-staged"}
		a.Services["web"] = s
	}
	return a
}
func (f *workflowFlow) selected(a *config.App, service, op string, apply bool) (Response, error) {
	q := f.req
	q.Config = a
	q.Service = service
	q.Operation = strings.Repeat(op, 32)
	q.Apply = apply
	selected := a
	if service != "" {
		selected = &config.App{Name: a.Name, Services: map[string]config.Service{service: a.Services[service]}}
	}
	return f.m.Execute(context.Background(), q, f.bundle(selected))
}
func (f *workflowFlow) bootstrap() {
	f.t.Helper()
	if _, err := f.selected(workflowApp(true), "db", "1", false); err != nil {
		f.t.Fatal(err)
	}
	j := f.load()
	if j.Version != 4 || j.Scope != "db" || len(j.Config.Services) != 1 || len(j.Completed.Services) != 1 || j.Completed.Services["db"] == nil || f.builds != 0 || f.pulls != 1 {
		f.t.Fatalf("bad scoped bootstrap: %+v", j)
	}
	r := j.ServiceStates["db"].Current
	v := f.docker.containers[r.Container]
	if r.Image != f.tag || r.VolumeID == "" || len(v.Mounts) != 1 || !strings.Contains(string(v.Mounts[0]), `"Destination":"/var/lib/postgresql/data"`) {
		f.t.Fatalf("missing immutable image/owned mount: %+v", r)
	}
}
func workflowFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestWorkflowBootstrapWholeDeployAndManualPreview(t *testing.T) {
	f := newWorkflowFlow(t)
	f.bootstrap()
	db := f.load().ServiceStates["db"].Current
	f.tag = "sha256:" + strings.Repeat("b", 64)
	if _, err := f.selected(workflowApp(false), "", "2", false); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if !reflect.DeepEqual(db, j.ServiceStates["db"].Current) || !j.ServiceStates["db"].Skipped || f.pulls != 1 || f.builds != 1 || len(j.Completed.Services) != 2 {
		t.Fatal("whole deploy refreshed protected db")
	}
	before := workflowFiles(t, f.m.dir(j.App))
	events := len(f.docker.events)
	resp, err := f.selected(workflowApp(true), "db", "3", false)
	if err != nil || resp.DeployPreview == nil || !resp.DeployPreview.NeedsConfirmation || resp.DeployPreview.CurrentImage != db.Image {
		t.Fatalf("preview: %+v %v", resp, err)
	}
	if f.pulls != 1 || len(f.docker.events) != events || !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(j.App))) {
		t.Fatal("preview mutated state")
	}
	if _, err = f.selected(workflowApp(true), "db", "3", true); err != nil {
		t.Fatal(err)
	}
	j = f.load()
	old := f.docker.containers[db.Container]
	if j.ServiceStates["db"].Current.Image != f.tag || f.pulls != 2 || old.State.Running || old.HostConfig.RestartPolicy.Name != "no" || j.ServiceStates["db"].VolumeID != db.VolumeID {
		t.Fatal("manual apply did not replace single writer")
	}
	if !reflect.DeepEqual(j.Config.Services["web"], *j.ServiceStates["web"].Current.Config) || !reflect.DeepEqual(j.Config.Services["web"], workflowApp(false).Services["web"]) {
		t.Fatal("pending unselected config captured")
	}
}
func TestExplicitPreviewCannotCreateMissingService(t *testing.T) {
	f := newWorkflowFlow(t)
	f.req.PreviewOnly = true
	_, err := f.selected(workflowApp(true), "db", "1", false)
	if err == nil || f.pulls != 0 || f.builds != 0 {
		t.Fatalf("preview started deployment: pulls=%d builds=%d err=%v", f.pulls, f.builds, err)
	}
}

func TestWorkflowProtectedChangesFailBeforeCapture(t *testing.T) {
	for _, kind := range []string{"env", "image", "policy", "secret"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkflowFlow(t)
			a := workflowApp(false)
			if kind == "secret" {
				s := a.Services["db"]
				s.Secrets = map[string]string{"PASSWORD": "credential"}
				a.Services["db"] = s
				stageMultiSecret(t, f.multiFlow, strings.Repeat("a", 32), "first")
			}
			if _, err := f.selected(a, "db", "1", false); err != nil {
				t.Fatal(err)
			}
			s := a.Services["db"]
			switch kind {
			case "env":
				s.Env = map[string]string{"CHANGE": "yes"}
			case "image":
				s.Image = "postgres:18"
			case "policy":
				s.Update = config.UpdateAuto
			case "secret":
				stageMultiSecret(t, f.multiFlow, strings.Repeat("b", 32), "second")
			}
			a.Services["db"] = s
			before := workflowFiles(t, f.m.dir(a.Name))
			events := len(f.docker.events)
			resp, err := f.selected(a, "", "2", false)
			if err == nil || strings.Contains(err.Error(), "vamos deploy") || resp.ProtectedService(a) != "db" {
				t.Fatalf("unsafe or untyped protection: %+v %v", resp, err)
			}
			f.req.PreviewOnly = true
			preview, err := f.selected(a, resp.ProtectedService(a), "3", false)
			if err != nil || preview.DeployPreview == nil || !preview.DeployPreview.NeedsConfirmation {
				t.Fatalf("missing actionable protection: %v", err)
			}
			if !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(a.Name))) || len(f.docker.events) != events || f.pulls != 1 || f.builds != 0 {
				t.Fatal("preflight changed captured/runtime state")
			}
		})
	}
}
func TestWorkflowResumePinsCapturedPullAndRejectsScopeChange(t *testing.T) {
	f := newWorkflowFlow(t)
	f.failInspect = true
	if _, err := f.selected(workflowApp(true), "db", "1", false); err == nil {
		t.Fatal("injected outage ignored")
	}
	pinned := f.tag
	before := workflowFiles(t, f.m.dir(f.req.App))
	q := f.req
	q.Config = workflowApp(false)
	q.Operation = strings.Repeat("2", 32)
	q.Supersede = strings.Repeat("1", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("scope-changing supersede: %v", err)
	}
	if !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(q.App))) {
		t.Fatal("rejected supersede mutated state")
	}
	f.failInspect = false
	f.tag = "sha256:" + strings.Repeat("b", 64)
	if _, err := f.resume(strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	if f.pulls != 1 || f.load().ServiceStates["db"].Current.Image != pinned {
		t.Fatal("resume refreshed mutable tag")
	}
}
func TestWorkflowStrictJournalValidation(t *testing.T) {
	f := newWorkflowFlow(t)
	f.bootstrap()
	if _, err := f.selected(workflowApp(false), "web", "2", false); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(f.load())
	for _, kind := range []string{"v1", "v2", "v3", "skipped", "image", "scope"} {
		t.Run(kind, func(t *testing.T) {
			var j journal
			if err := json.Unmarshal(original, &j); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "v1":
				j.Version = 1
			case "v2":
				j.Version = 2
			case "v3":
				j.Version = 3
			case "skipped":
				j.ServiceStates["db"].Skipped = false
			case "image":
				j.ServiceStates["web"].Current.Image = "sha256:" + strings.Repeat("f", 64)
			case "scope":
				j.Scope = "unknown"
			}
			if err := validateMultiJournal(&j, j.App); err == nil {
				t.Fatal("tampered journal accepted")
			}
		})
	}
}
func TestWorkflowUnknownSelectorRejected(t *testing.T) {
	f := newWorkflowFlow(t)
	f.bootstrap()
	before := workflowFiles(t, f.m.dir(f.req.App))
	q := f.req
	q.Config = workflowApp(false)
	q.Service = "unknown"
	q.Operation = strings.Repeat("2", 32)
	if _, err := f.m.Execute(context.Background(), q, nil); err == nil {
		t.Fatal("unknown selector accepted")
	}
	if !reflect.DeepEqual(before, workflowFiles(t, f.m.dir(q.App))) || f.pulls != 1 {
		t.Fatal("invalid selector changed state")
	}
}

func TestWorkflowExecuteMigrationPreservesDeployment(t *testing.T) {
	f := newWorkflowFlow(t)
	f.bootstrap()
	q := f.req
	q.Action = "run"
	q.Service = "web"
	q.Operation = strings.Repeat("2", 32)
	q.Config = workflowApp(false)
	q.Command = []string{"./migrate", "--once"}
	selected := &config.App{Name: q.App, Services: map[string]config.Service{"web": q.Config.Services["web"]}}
	before, _ := json.Marshal(f.load())
	base := f.m.Run
	complete := false
	f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" {
			if a[0] == "start" && f.docker.containers["vamos-run-"+q.Operation] != nil {
				f.docker.containers["vamos-run-"+q.Operation].State.Running = true
				return process.Result{}, nil
			}
			if a[0] == "wait" {
				complete = true
				f.docker.containers["vamos-run-"+q.Operation].State.Running = false
				return process.Result{Stdout: []byte("0\n")}, nil
			}
			if complete && len(a) == 3 && a[0] == "container" && a[1] == "inspect" {
				v := f.docker.containers["vamos-run-"+q.Operation]
				if a[2] == v.ID || a[2] == strings.TrimPrefix(v.Name, "/") {
					raw, _ := json.Marshal(v)
					var obj map[string]any
					_ = json.Unmarshal(raw, &obj)
					s := obj["State"].(map[string]any)
					s["Status"] = "exited"
					s["ExitCode"] = 0
					s["FinishedAt"] = "2026-06-01T00:01:00Z"
					return dockerJSON([]any{obj})
				}
			}
		}
		return base(ctx, n, a, b)
	}
	resp, err := f.m.Execute(context.Background(), q, f.bundle(selected))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Run == nil || resp.Run.Phase != "complete" || resp.Run.ExitCode == nil || *resp.Run.ExitCode != 0 {
		t.Fatalf("migration outcome: %+v", resp.Run)
	}
	after, _ := json.Marshal(f.load())
	if string(before) != string(after) {
		t.Fatal("migration rewrote deployment journal")
	}
	for _, action := range []string{"status", "logs"} {
		r := f.req
		r.Action = action
		r.Operation = ""
		r.Config = nil
		r.ContextFormat = ""
		r.RunOperation = q.Operation
		if _, err := f.m.Execute(context.Background(), r, nil); err != nil {
			t.Fatalf("%s --run: %v", action, err)
		}
	}
	if _, err := f.selected(workflowApp(false), "", "3", false); err != nil {
		t.Fatalf("completed run endpoint blocked next deploy: %v", err)
	}
	if f.pulls != 1 {
		t.Fatal("migration/next deploy refreshed db")
	}
}
