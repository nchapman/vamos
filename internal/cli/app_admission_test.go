package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/agentclient"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/transport"
)

// Execute the displayed POSIX command with a local argv-capturing function,
// then pass its arguments through the real parser. No SSH or subprocess CLI.
func TestProtectedFollowupIsScopedPreview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	path, store, deps := appFixture(t)
	if err := store.Put(registry.Server{Name: "staging", Target: "runner@staging", Domain: "staging.example.com"}); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(filepath.Dir(path), "prod user's $(false).yaml")
	if err := os.Rename(path, manifest); err != nil {
		t.Fatal(err)
	}
	deps.Archive = func(p string, _ config.Build) ([]byte, error) {
		if p != manifest {
			t.Fatal(p)
		}
		return []byte("tar"), nil
	}
	deps.Run = func(_ context.Context, target string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
		if target != "runner@staging" || r.App != "hello" {
			t.Fatal(target, r.App)
		}
		if r.Service == "" {
			return deploy.Response{Version: 1, Error: "protected", ErrorCode: "protected_change:web"}, errors.New("protected")
		}
		if r.Service != "web" || r.Apply || !r.PreviewOnly || !r.ReuseData {
			t.Fatal("wrong follow-up request", r)
		}
		return deploy.Response{Version: 1, DeployPreview: &deploy.DeploymentPreview{Service: "web", NeedsConfirmation: true}}, nil
	}
	var out, diag bytes.Buffer
	if code := appCommand(context.Background(), "deploy", []string{"--server", "staging", "-f", manifest, "--reuse-data", "--executor", "my executor", "--timeout", "3m"}, &out, &diag, deps); code != 1 {
		t.Fatal(code)
	}
	_, cmd, ok := strings.Cut(diag.String(), "Preview protected update: ")
	if !ok {
		t.Fatal(diag.String())
	}
	data, err := exec.Command("sh", "-c", `vamos() { printf '%s\0' "$@"; }; `+strings.TrimSpace(cmd)).Output()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if args[0] != "deploy" || !strings.Contains(cmd, "--server staging") || !strings.Contains(cmd, "--timeout 3m0s") || !strings.Contains(cmd, "--executor 'my executor'") {
		t.Fatal(args)
	}
	if code := appCommand(context.Background(), args[0], args[1:], &out, &diag, deps); code != 0 {
		t.Fatalf("%d: %s", code, &diag)
	}
}

func TestGuardedPreviewNamesTargetWithoutAnnouncingDeployment(t *testing.T) {
	path, _, deps := appFixture(t)
	for _, out := range []interface {
		io.Writer
		String() string
	}{&bytes.Buffer{}, newHelpAppsTerminal(160, false)} {
		deps.Agent = func(_ context.Context, _ string, r deploy.Request, _ []byte) (deploy.Response, error) {
			text := strings.ToLower(out.String())
			if !strings.Contains(text, "preview only") || !strings.Contains(text, "home") || strings.Contains(text, "requested operation") || strings.Contains(text, "(operation ") {
				t.Error(out.String())
			}
			return deploy.Response{Version: 1, DeployPreview: &deploy.DeploymentPreview{Service: r.Service, NeedsConfirmation: true}}, nil
		}
		if code := appCommand(context.Background(), "deploy", []string{"--preview", "--service", "web", "-f", path}, out, io.Discard, deps); code != 0 {
			t.Fatal(code)
		}
	}
}

func TestAppRecoveryRequiresObservationOrMatchingRecord(t *testing.T) {
	path, _, deps := appFixture(t)
	for _, tc := range []struct {
		action, mode    string
		unknown, resume bool
	}{
		{"deploy", "lost", true, true}, {"deploy", "other-record", false, false}, {"deploy", "matching", false, true}, {"status", "lost", false, false}, {"destroy", "lost", false, false}, {"deploy", "queue-failed", false, false},
	} {
		t.Run(tc.action+"_"+tc.mode, func(t *testing.T) {
			deps.Agent = func(_ context.Context, _ string, r deploy.Request, _ []byte) (deploy.Response, error) {
				e := errors.New("readiness: request not submitted") // Text alone proves neither admission nor readiness failure.
				switch tc.mode {
				case "queue-failed":
					return deploy.Response{}, transport.ErrOperationFailed
				case "matching", "other-record":
					op := r.Operation
					if tc.mode == "other-record" {
						op = "another"
					}
					return deploy.Response{Version: 1, Error: "readiness", Status: &deploy.Status{Operation: op}}, e
				default:
					return deploy.Response{}, e
				}
			}
			for _, diag := range []interface {
				io.Writer
				String() string
			}{&bytes.Buffer{}, newHelpAppsTerminal(100, false)} {
				var out bytes.Buffer
				code := appCommand(context.Background(), tc.action, []string{"-f", path}, &out, diag, deps)
				if code != 1 || strings.Contains(diag.String(), "Outcome unknown") != tc.unknown || strings.Contains(diag.String(), "vamos resume") != tc.resume || strings.Contains(diag.String(), "stop-first services") != (tc.mode == "matching") {
					t.Fatal(diag.String())
				}
			}
		})
	}
}

func TestSubmissionRefusalNoRecovery(t *testing.T) {
	path, _, deps := appFixture(t)
	for _, class := range []string{"forbidden", "not_submitted"} {
		deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
			return deploy.Response{}, transport.NotSubmitted(&agentclient.RemoteError{Code: class, Message: "request denied"})
		}
		for _, diag := range []interface {
			io.Writer
			String() string
		}{&bytes.Buffer{}, newHelpAppsTerminal(100, false)} {
			var out bytes.Buffer
			code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, diag, deps)
			if code != 1 || !strings.Contains(diag.String(), "not submitted") || strings.Contains(diag.String(), "vamos status") || strings.Contains(diag.String(), "resume") {
				t.Errorf("%s: %s", class, diag.String())
			}
		}
	}
}
