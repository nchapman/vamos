package cli

import (
	"bytes"
	"context"
	"github.com/nchapman/vamos/internal/deploy"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestServiceBindingsTargetIsolation(t *testing.T) {
	dir := t.TempDir()
	want := serviceBindings{"web": {"host-a", "inc-a"}}
	if err := writeServiceBindings(dir, "root@a", "app", want); err != nil {
		t.Fatal(err)
	}
	got, err := readServiceBindings(dir, "root@a", "app")
	if err != nil || got["web"] != want["web"] {
		t.Fatalf("%v %v", got, err)
	}
	for _, target := range []string{"root@b", "../escape"} {
		got, err := readServiceBindings(dir, target, "app")
		if err != nil || len(got) != 0 {
			t.Fatalf("target isolation: %v %v", got, err)
		}
	}
	got, err = readServiceBindings(t.TempDir(), "root@a", "app")
	if err != nil || len(got) != 0 {
		t.Fatalf("directory isolation: %v %v", got, err)
	}
	// Rebinding must replace an existing metadata file on every client OS.
	if err := writeServiceBindings(dir, "root@a", "app", serviceBindings{"web": {"host-a", "inc-new"}, "worker": {"host-a", "inc-worker"}}); err != nil {
		t.Fatal(err)
	}
	got, err = readServiceBindings(dir, "root@a", "app")
	if err != nil || got["web"].Incarnation != "inc-new" || got["worker"].Incarnation != "inc-worker" {
		t.Fatalf("rebinding: %v %v", got, err)
	}
	info, err := os.Stat(serviceBindingPath(dir, "root@a", "app"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("permissions: %v %v", info, err)
	}
}
func TestServiceBindingsRejectSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".vamos")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := writeServiceBindings(dir, "target", "app", serviceBindings{"web": {"host", "inc"}}); err == nil {
		t.Fatal("accepted symlink")
	}
}
func TestAcknowledgedBindings(t *testing.T) {
	s := &deploy.Status{Operation: "old", HostIdentity: "host", Services: map[string]deploy.ServiceStatus{"web": {Incarnation: "inc"}, "other": {Incarnation: "other"}}}
	r := deploy.Request{Operation: "new"}
	if got := acknowledgedBindings(r, s, []string{"web"}); len(got) != 0 {
		t.Fatal("preflight failure adopted existing service")
	}
	s.Operation = "new"
	if got := acknowledgedBindings(r, s, []string{"web"}); len(got) != 1 || got["web"].Incarnation != "inc" {
		t.Fatalf("first creation: %v", got)
	}
	s.Operation = "old"
	r.HostIdentity = "host"
	r.Bindings = map[string]string{"web": "inc"}
	if got := acknowledgedBindings(r, s, []string{"web"}); len(got) != 1 {
		t.Fatal("matching binding not acknowledged")
	}
	r.HostIdentity = "stale"
	if got := acknowledgedBindings(r, s, []string{"web"}); len(got) != 0 {
		t.Fatal("stale host adopted")
	}
	r.HostIdentity = "host"
	r.Bindings["web"] = "stale"
	if got := acknowledgedBindings(r, s, []string{"web"}); len(got) != 0 {
		t.Fatal("stale incarnation adopted")
	}
}

func TestServiceBindOnlyReadsStatus(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, archive []byte) (deploy.Response, error) {
		if r.Action != "status" || r.Service != "web" || len(archive) != 0 || r.Config != nil {
			t.Fatalf("bind mutated: %+v", r)
		}
		return deploy.Response{Status: &deploy.Status{HostIdentity: "host", Services: map[string]deploy.ServiceStatus{"web": {Incarnation: "inc"}}}}, nil
	}
	var out, diag bytes.Buffer
	if code := serviceCommand(context.Background(), []string{"bind", "web", "-f", path, "--server", "home"}, &out, &diag, deps); code != 0 {
		t.Fatalf("%d: %s", code, &diag)
	}
	got, err := readServiceBindings(filepath.Dir(path), "root@home", "hello")
	if err != nil || got["web"].Incarnation != "inc" {
		t.Fatalf("%v %v", got, err)
	}
	bound, err := readBinding(filepath.Dir(path))
	if err != nil || bound == nil || bound.Server != "home" || bound.App != "hello" {
		t.Fatalf("expected service bind with manifest to bind unbound project, got %+v %v", bound, err)
	}
}
func TestReAddRequiresScopedApproval(t *testing.T) {
	for _, args := range [][]string{{"--re-add"}, {"--re-add", "--service", "web"}, {"--re-add", "--yes"}} {
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "deploy", args, &out, &diag, appDeps{}); code != 2 {
			t.Fatalf("%v: %d %s", args, code, &diag)
		}
	}
}

func TestDeployFirstCreationAcknowledgesIdentity(t *testing.T) {
	path, _, deps := appFixture(t)
	calls := 0
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
		calls++
		if !r.Partial || !r.ReAdd || !r.Apply || r.ReuseData {
			t.Fatalf("wrong lifecycle flags: %+v", r)
		}
		if calls == 1 && len(r.Bindings) != 0 {
			t.Fatal("new service already bound")
		}
		if calls == 2 && (r.HostIdentity != "11111111111111111111111111111111" || r.Bindings["web"] != "22222222222222222222222222222222") {
			t.Fatal("acknowledgment not sent")
		}
		return deploy.Response{Status: &deploy.Status{Operation: r.Operation, HostIdentity: "11111111111111111111111111111111", Services: map[string]deploy.ServiceStatus{"web": {Incarnation: "22222222222222222222222222222222"}}}}, nil
	}
	for i := 0; i < 2; i++ {
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "deploy", []string{"-f", path, "--service", "web", "--yes", "--re-add"}, &out, &diag, deps); code != 0 {
			t.Fatalf("%d %s", code, &diag)
		}
	}
}
