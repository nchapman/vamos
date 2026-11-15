package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/registry"
)

func TestApprovalPreservesManifestTargetAndOptions(t *testing.T) {
	path, store, deps := appFixture(t)
	renamed := filepath.Join(filepath.Dir(path), "staging user's.yaml")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(registry.Server{Name: "staging", Target: "root@staging", Domain: "staging.example.com"}); err != nil {
		t.Fatal(err)
	}
	deps.Archive = func(filename string, _ config.Build) ([]byte, error) {
		if filename != renamed {
			t.Fatalf("wrong manifest: %s", filename)
		}
		return []byte("tar"), nil
	}
	deps.Run = func(_ context.Context, target string, _ deploy.Request, _, _ []byte) (deploy.Response, error) {
		if target != "root@staging" {
			t.Fatal(target)
		}
		return deploy.Response{Version: 1, DeployPreview: &deploy.DeploymentPreview{Service: "web", NeedsConfirmation: true}}, nil
	}
	var out, diagnostic bytes.Buffer
	args := []string{"-f", renamed, "--server", "staging", "--service", "web", "--timeout", "3m", "--executor", "custom executor", "--reuse-data"}
	if code := appCommand(context.Background(), "deploy", args, &out, &diagnostic, deps); code != 0 {
		t.Fatalf("code=%d: %s", code, &diagnostic)
	}
	_, command, ok := strings.Cut(out.String(), "Approve: ")
	if !ok {
		t.Fatal(&out)
	}
	quoted := "'" + strings.ReplaceAll(renamed, "'", "'\\''") + "'"
	if runtime.GOOS == "windows" {
		quoted = "'" + strings.ReplaceAll(renamed, "'", "''") + "'"
	}
	for _, want := range []string{"vamos deploy --service web --yes", "-f " + quoted, "--server staging", "--timeout 3m0s", "--executor 'custom executor'", "--reuse-data"} {
		if !strings.Contains(command, want) {
			t.Errorf("suggestion missing %q: %s", want, command)
		}
	}
}

func TestDeploymentFailuresDoNotInventAdoptionAdvice(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{}, errors.New("build failed")
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diagnostic, deps); code != 1 {
		t.Fatal(code)
	}
	if strings.Contains(diagnostic.String(), "service bind") {
		t.Fatalf("unrelated failure suggests adoption: %s", &diagnostic)
	}
}

func TestBindingAdviceUsesManifestDirectoryAndSelectedServer(t *testing.T) {
	path, store, deps := appFixture(t)
	t.Chdir(t.TempDir())
	if err := store.Put(registry.Server{Name: "staging", Target: "root@staging", Domain: "staging.example.com"}); err != nil {
		t.Fatal(err)
	}
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Error: "service web requires adoption", ErrorCode: deploy.CodeBindingRequired}, errors.New("service web requires adoption")
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"-f", path, "--server", "staging", "--service", "web"}, &out, &diagnostic, deps); code != 1 {
		t.Fatal(code)
	}
	want := suggestedCommand("vamos", "service", "bind", "web", "-f", path, "--server", "staging")
	if !strings.Contains(diagnostic.String(), want) || strings.Contains(diagnostic.String(), "bind web --app") {
		t.Fatal(&diagnostic)
	}
}

func TestConfigFailureExplainsExplicitTargetRecovery(t *testing.T) {
	for _, action := range []string{"status", "logs", "resume", "rollback"} {
		for _, missing := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "/invalid", true: "/missing"}[missing], func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "broken.yaml")
				if !missing {
					if err := os.WriteFile(path, []byte("invalid: ["), 0600); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"-f", path}
				if action == "resume" {
					args = append(args, strings.Repeat("a", 32))
				}
				var out, diagnostic bytes.Buffer
				if code := appCommand(context.Background(), action, args, &out, &diagnostic, appDeps{}); code != 1 {
					t.Fatal(code)
				}
				if !strings.Contains(diagnostic.String(), "vamos "+action+" --server SERVER --app APP") {
					t.Fatal(&diagnostic)
				}
			})
		}
	}
	for _, action := range []string{"deploy", "run"} {
		args := []string{"-f", filepath.Join(t.TempDir(), "missing.yaml")}
		if action == "run" {
			args = append(args, "--service", "web", "--", "true")
		}
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), action, args, &out, &diagnostic, appDeps{}); code != 1 {
			t.Fatal(code)
		}
		if strings.Contains(diagnostic.String(), "--app APP") {
			t.Fatalf("suggested bypassing required config: %s", &diagnostic)
		}
	}
}

func TestSnapshotLogsKeepMetadataOffStdout(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Logs: "workload output\n", LogService: "web", LogContainer: "container", LogCandidate: true}, nil
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "logs", []string{"-f", path}, &out, &diagnostic, deps); code != 0 {
		t.Fatal(code, &diagnostic)
	}
	if out.String() != "workload output\n" || !strings.Contains(diagnostic.String(), `Logs: service="web" container="container" candidate=true`) {
		t.Fatalf("stdout=%q stderr=%q", &out, &diagnostic)
	}
	if code := appCommand(context.Background(), "logs", []string{"-f", path}, &out, brokenWriter{}, deps); code != 1 {
		t.Fatal("ignored diagnostic output failure")
	}
}

func TestAppHelpIsCommandSpecificAndComplete(t *testing.T) {
	for _, tc := range []struct{ action, usage, flag, absent string }{
		{"deploy", "vamos deploy", "-timeout", "vamos destroy"},
		{"logs", "vamos logs", "-follow", "-re-add"},
		{"resume", "vamos resume", "-timeout", "-supersede"},
		{"service-remove", "vamos service remove", "-json", "-reuse-data"},
		{"service-bind", "vamos service bind", "-timeout", "-yes"},
		{"run", "vamos run", "-service", "-follow"},
		{"rollback", "vamos rollback", "-allow-downtime", "-re-add"},
		{"status", "vamos status", "-json", "-yes"},
		{"cleanup", "vamos cleanup", "-json", "-service"},
		{"destroy", "vamos destroy", "-yes", "-candidate"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if code := appCommand(context.Background(), tc.action, []string{"--help"}, &out, &diagnostic, appDeps{}); code != 0 {
				t.Fatal(code)
			}
			if !strings.Contains(out.String(), tc.usage) || !strings.Contains(out.String(), tc.flag) || strings.Contains(out.String(), tc.absent) || diagnostic.Len() != 0 {
				t.Fatalf("stdout=%s stderr=%s", &out, &diagnostic)
			}
		})
	}
}
