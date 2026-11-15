package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/registry"
)

func appFixture(t *testing.T) (string, *registry.Store, appDeps) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vamos.yaml")
	if err := os.WriteFile(path, []byte("app: hello\nservices:\n  web:\n    build: {context: ., dockerfile: Dockerfile}\n    http: {port: 80, health: /healthz}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := registry.New(t.TempDir())
	if err := store.Put(registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}
	deps := appDeps{
		Registry: func() (*registry.Store, error) { return store, nil },
		Archive: func(filename string, b config.Build) ([]byte, error) {
			if filename != path || b.Dockerfile != "Dockerfile" {
				t.Fatal("wrong context input")
			}
			return []byte("tar"), nil
		},
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("binary"), nil },
	}
	return path, store, deps
}

func TestDeployCapturesRequestAndBindsDefault(t *testing.T) {
	path, store, deps := appFixture(t)
	var seen deploy.Request
	deps.Run = func(_ context.Context, target string, r deploy.Request, b, a []byte) (deploy.Response, error) {
		seen = r
		payloads, err := buildcontext.ReadBundle(a, r.Config)
		if err != nil || r.ContextFormat != "bundle-v1" || len(payloads) != 1 {
			t.Fatalf("invalid bundle: %v", err)
		}
		if target != "root@home" || string(b) != "binary" {
			t.Fatal("invalid transport inputs")
		}
		return deploy.Response{Version: 1, Status: &deploy.Status{App: r.App, Operation: r.Operation, Phase: "ready", Running: true}}, nil
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diagnostic, deps); code != 0 {
		t.Fatalf("code=%d error=%s", code, &diagnostic)
	}
	if err := deploy.ValidateRequest(seen); err != nil {
		t.Fatal(err)
	}
	bound, err := readBinding(filepath.Dir(path))
	if err != nil || bound == nil || bound.Server != "home" || bound.App != "hello" {
		t.Fatalf("binding=%+v err=%v", bound, err)
	}
	ignore, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".vamos", ".gitignore"))
	if err != nil || string(ignore) != "*\n" {
		t.Fatal("binding not Git-ignored")
	}
	if err := store.Put(registry.Server{Name: "other", Target: "root@other", Domain: "other.example.com"}); err != nil {
		t.Fatal(err)
	}
	deps.Run = func(_ context.Context, target string, r deploy.Request, b, a []byte) (deploy.Response, error) {
		if target != "root@other" {
			t.Fatal("override ignored")
		}
		return deploy.Response{Version: 1, Status: &deploy.Status{App: r.App}}, nil
	}
	if code := appCommand(context.Background(), "status", []string{"-f", path, "--server", "other"}, &out, &diagnostic, deps); code != 0 {
		t.Fatal(&diagnostic)
	}
	after, err := readBinding(filepath.Dir(path))
	if err != nil || *after != *bound {
		t.Fatal("explicit override rewrote binding")
	}
}

func TestDeployWithServerFlagBindsUnboundProject(t *testing.T) {
	path, store, deps := appFixture(t)
	if err := store.Put(registry.Server{Name: "staging", Target: "root@staging", Domain: "staging.example.com"}); err != nil {
		t.Fatal(err)
	}
	var targets []string
	deps.Run = func(_ context.Context, target string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
		targets = append(targets, target)
		return deploy.Response{Version: 1, Status: &deploy.Status{App: r.App, Operation: r.Operation, Phase: "ready", Running: true}}, nil
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"-f", path, "--server", "staging"}, &out, &diagnostic, deps); code != 0 {
		t.Fatalf("deploy code=%d error=%s", code, &diagnostic)
	}
	bound, err := readBinding(filepath.Dir(path))
	if err != nil || bound == nil || bound.Server != "staging" || bound.Target != "root@staging" {
		t.Fatalf("expected unbound project to bind to staging on first deploy, got binding=%+v err=%v", bound, err)
	}
	if code := appCommand(context.Background(), "status", []string{"-f", path}, &out, &diagnostic, deps); code != 0 {
		t.Fatalf("status code=%d error=%s", code, &diagnostic)
	}
	if len(targets) != 2 || targets[0] != "root@staging" || targets[1] != "root@staging" {
		t.Fatalf("expected subsequent command without --server to use bound server root@staging, got %v", targets)
	}
}

func TestResumeUsesOnlyCapturedOperation(t *testing.T) {
	_, _, deps := appFixture(t)
	deps.Archive = func(string, config.Build) ([]byte, error) { t.Fatal("resume archived current tree"); return nil, nil }
	id := strings.Repeat("a", 32)
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, archive []byte) (deploy.Response, error) {
		if r.Config != nil || len(archive) != 0 || r.Operation != id || r.Action != "resume" {
			t.Fatalf("request=%+v", r)
		}
		return deploy.Response{Version: 1, Status: &deploy.Status{App: "hello", Operation: id, Phase: "ready", Running: true}}, nil
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "resume", []string{"--server", "home", "--app", "hello", id}, &out, &diagnostic, deps); code != 0 {
		t.Fatalf("code=%d error=%s", code, &diagnostic)
	}
}

func TestAppRefusalsBeforeRemoteContact(t *testing.T) {
	for _, args := range [][]string{{"--service", "db"}, {"--timeout", "0"}, {"--app", "hello"}, {"--json"}, {"--supersede", "wrong-id"}} {
		path, _, deps := appFixture(t)
		deps.Inspect = nil
		deps.Run = nil
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), "deploy", append([]string{"-f", path}, args...), &out, &diagnostic, deps); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Run("invalid service config", func(t *testing.T) {
		path, _, deps := appFixture(t)
		os.WriteFile(path, []byte("app: hello\nservices:\n  db: {}\n"), 0600)
		deps.Archive = nil
		deps.Inspect = nil
		deps.Run = nil
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diagnostic, deps); code != 1 {
			t.Fatal(code)
		}
	})
}

func TestAppOutputAndRemoteFailures(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{}, errors.New("failed remotely")
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diagnostic, deps); code != 1 || !strings.Contains(diagnostic.String(), "vamos resume") {
		t.Fatalf("code=%d err=%s", code, &diagnostic)
	}
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Status: &deploy.Status{App: "hello"}, Logs: "hello\n"}, nil
	}
	for _, action := range []string{"status", "logs"} {
		if code := appCommand(context.Background(), action, []string{"-f", path}, brokenWriter{}, &diagnostic, deps); code != 1 {
			t.Fatal("ignored output failure", action, code)
		}
	}
}

func TestProjectMetadataReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectMetadata(path); err == nil {
		t.Fatal("accepted oversized project metadata")
	}
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readProjectMetadata(path); err != nil || string(data) != "small" {
		t.Fatal("bounded read failed", err)
	}
}

func TestBindingRefusesRetargetAndCorruptMetadata(t *testing.T) {
	path, store, _ := appFixture(t)
	dir := filepath.Dir(path)
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := bindProject(dir, "hello", state.Servers["home"]); err != nil {
		t.Fatal(err)
	}
	server := state.Servers["home"]
	server.Target = "root@different"
	state.Servers["home"] = server
	if _, err := selectAppServer(state, dir, "hello", ""); err == nil {
		t.Fatal("silently retargeted project")
	}
	if _, err := selectAppServer(state, dir, "renamed", ""); err == nil {
		t.Fatal("silently renamed app")
	}
	file := filepath.Join(dir, ".vamos", "server.json")
	if err := os.WriteFile(file, []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBinding(dir); err == nil {
		t.Fatal("accepted invalid binding")
	}
}

func TestMaintenancePreviewAndApply(t *testing.T) {
	for _, action := range []string{"cleanup", "destroy", "service-remove"} {
		for _, apply := range []bool{false, true} {
			for _, asJSON := range []bool{false, true} {
				path, _, deps := appFixture(t)
				deps.Archive = nil
				deps.Run = func(_ context.Context, _ string, r deploy.Request, b, a []byte) (deploy.Response, error) {
					if r.Apply != apply || len(r.Operation) != 32 || r.Config != nil || len(a) != 0 || len(b) == 0 {
						t.Fatalf("unsafe request: %+v", r)
					}
					response := deploy.Response{Version: 1, Plan: &deploy.Plan{Action: action, App: "hello", Containers: []string{"old-container"}, Images: []string{"sha256:old"}, Archives: []string{"old.tar"}, Network: "app-net", Retained: []string{"shared image: used elsewhere"}}}
					if apply {
						response.Status = &deploy.Status{Phase: "ready"}
						if action == "destroy" {
							response.Status.Phase = "destroyed"
						}
					}
					return response, nil
				}
				args := []string{"-f", path}
				if action == "service-remove" {
					args = append(args, "--service", "web")
				}
				if apply {
					args = append(args, "--yes")
				}
				if asJSON {
					args = append(args, "--json")
				}
				var out, diag bytes.Buffer
				if code := appCommand(context.Background(), action, args, &out, &diag, deps); code != 0 {
					t.Fatalf("%d: %s", code, &diag)
				}
				text := out.String()
				if asJSON {
					var r deploy.Response
					if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Plan == nil {
						t.Fatalf("full response JSON: %s", text)
					}
				} else {
					for _, resource := range []string{"old-container", "sha256:old", "old.tar", "app-net", "shared image: used elsewhere"} {
						if !strings.Contains(text, resource) {
							t.Fatal(text)
						}
					}
					if !apply && !strings.Contains(text, "no resources deleted") {
						t.Fatal(text)
					}
					if strings.Contains(text, "Route configured") || strings.Contains(text, "https://") {
						t.Fatal(text)
					}
				}
			}
		}
	}
}

func TestDestroyedStatusPreservesJSONShape(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Status: &deploy.Status{App: "hello", Phase: "destroyed"}}, nil
	}
	var out, diag bytes.Buffer
	if code := appCommand(context.Background(), "status", []string{"-f", path, "--json"}, &out, &diag, deps); code != 0 {
		t.Fatalf("%d: %s", code, &diag)
	}
	var status deploy.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Phase != "destroyed" {
		t.Fatalf("status JSON changed shape: %s", &out)
	}
}

func TestYesRejectedBeforeContact(t *testing.T) {
	for _, action := range []string{"deploy", "resume", "status", "logs"} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), action, []string{"--yes"}, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("%s: %d", action, code)
		}
	}
}
