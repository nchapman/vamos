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

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

const testDeployKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHJlcGxhY2VtZW50cHVibGlja2V5Ym9keWZvcnRlc3Rpbmdvbmx5 ci@host"

func TestLifecycleRefusals(t *testing.T) {
	for _, tt := range []struct {
		op   string
		args []string
	}{
		{"add", []string{"--domain", "example.com", "home", "root@home"}},
		{"teardown", []string{"--yes", "home", "root@home"}},
		{"add", []string{"--yes", "--purge", "home", "root@home"}},
		{"add", []string{"--yes", "--domain", "https://example.com", "home", "root@home"}},
		{"add", []string{"--yes", "--domain", "example.com", "BAD", "root@home"}},
		{"add", []string{"--yes", "--domain", "example.com", "home", "host;id"}},
		{"status", []string{"--timeout=0", "home"}},
	} {
		t.Run(tt.op+"/"+stringArgs(tt.args), func(t *testing.T) {
			store := registry.New(t.TempDir())
			deps := lifecycleDeps{Registry: func() (*registry.Store, error) { return store, nil }}
			// Nil external boundaries deliberately make any remote contact fail the test.
			var out, diagnostic bytes.Buffer
			if code := serverLifecycle(context.Background(), tt.op, tt.args, &out, &diagnostic, deps); code != 2 || diagnostic.Len() == 0 {
				t.Fatalf("code=%d diagnostic=%s", code, &diagnostic)
			}
		})
	}
}

type footerFailureWriter struct{ calls int }

func (w *footerFailureWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 3 {
		return 0, errors.New("stdout closed")
	}
	return len(p), nil
}

func TestLifecycleFooterWriteFailure(t *testing.T) {
	for _, op := range []string{"add", "teardown"} {
		t.Run(op, func(t *testing.T) {
			store := registry.New(t.TempDir())
			config := lifecycle.Config{Name: "home", Domain: "example.com"}
			phase := "ready"
			if op == "teardown" {
				phase = "removed"
			}
			deps := lifecycleDeps{
				Registry:  func() (*registry.Store, error) { return store, nil },
				DeployKey: func() (string, error) { return testDeployKey, nil },
				Inspect: func(context.Context, string) (*host.Report, error) {
					return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
				},
				Artifact: func(string, string) ([]byte, error) { return []byte("binary"), nil },
				Status: func(context.Context, string) (*ssh.Status, error) {
					return &ssh.Status{State: &lifecycle.State{Config: config}}, nil
				},
				Run: func(context.Context, string, executor.Request, []byte) (*lifecycle.State, error) {
					return &lifecycle.State{Phase: phase, Config: config}, nil
				},
			}
			args := []string{"--yes"}
			if op == "add" {
				args = append(args, "--domain", "example.com")
			} else {
				args = append(args, "--purge")
			}
			args = append(args, "home", "root@home")
			var diagnostic bytes.Buffer
			if code := serverLifecycle(context.Background(), op, args, &footerFailureWriter{}, &diagnostic, deps); code != 1 {
				t.Fatalf("code=%d error=%s", code, &diagnostic)
			}
		})
	}
}

func stringArgs(args []string) string { b, _ := json.Marshal(args); return string(b) }

func TestLifecycleRegistration(t *testing.T) {
	for _, op := range []string{"add", "teardown"} {
		for _, outcome := range []string{"success", "remote error", "wrong phase"} {
			t.Run(op+"/"+outcome, func(t *testing.T) {
				store := registry.New(t.TempDir())
				server := registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}
				if op == "teardown" {
					if err := store.Put(server); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				deps := lifecycleDeps{
					Registry:  func() (*registry.Store, error) { return store, nil },
					DeployKey: func() (string, error) { return testDeployKey, nil },
					Status: func(context.Context, string) (*ssh.Status, error) {
						return &ssh.Status{State: &lifecycle.State{Config: lifecycle.Config{Name: server.Name, Domain: server.Domain}}}, nil
					},
					Inspect: func(ctx context.Context, target string) (*host.Report, error) {
						if target != server.Target {
							t.Fatal(target)
						}
						if _, ok := ctx.Deadline(); !ok {
							t.Fatal("missing deadline")
						}
						return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
					},
					Artifact: func(path, arch string) ([]byte, error) {
						if path != "artifact" || arch != "amd64" {
							t.Fatalf("artifact %s %s", path, arch)
						}
						return []byte("binary"), nil
					},
					Run: func(ctx context.Context, target string, r executor.Request, b []byte) (*lifecycle.State, error) {
						calls++
						want := "bootstrap"
						phase := "ready"
						wantKey := testDeployKey
						if op == "teardown" {
							want = "teardown"
							phase = "removed"
							wantKey = ""
						}
						if target != server.Target || r.Version != executor.Version || r.Operation != want || !r.Confirm || r.Purge != (op == "teardown") || r.Config == nil || *r.Config != (lifecycle.Config{Name: server.Name, Domain: server.Domain}) || string(b) != "binary" {
							t.Fatalf("unexpected request %+v", r)
						}
						if r.DeployKey != wantKey {
							t.Fatalf("deploy key not provisioned: %q", r.DeployKey)
						}
						if outcome == "remote error" {
							return nil, errors.New("remote failed")
						}
						if outcome == "wrong phase" {
							phase = "failed"
						}
						return &lifecycle.State{Phase: phase, ID: "installation"}, nil
					},
				}
				args := []string{"--yes", "--executor", "artifact"}
				if op == "add" {
					args = append(args, "--domain", server.Domain)
				} else {
					args = append(args, "--purge")
				}
				args = append(args, server.Name, server.Target)
				var out, diagnostic bytes.Buffer
				code := serverLifecycle(context.Background(), op, args, &out, &diagnostic, deps)
				if (code == 0) != (outcome == "success") || calls != 1 {
					t.Fatalf("code=%d calls=%d error=%s", code, calls, &diagnostic)
				}
				state, err := store.Read()
				if err != nil {
					t.Fatal(err)
				}
				_, present := state.Servers[server.Name]
				wantPresent := (op == "add" && outcome == "success") || (op == "teardown" && outcome != "success")
				if present != wantPresent {
					t.Fatalf("registry=%+v", state)
				}
			})
		}
	}
}

func TestLifecycleStatusAndTeardownOwnership(t *testing.T) {
	for _, status := range []*ssh.Status{{}, {Active: true}, {State: &lifecycle.State{Config: lifecycle.Config{Name: "other"}}}} {
		store := registry.New(t.TempDir())
		deps := lifecycleDeps{Registry: func() (*registry.Store, error) { return store, nil }, Status: func(context.Context, string) (*ssh.Status, error) { return status, nil }}
		var out, diagnostic bytes.Buffer
		if code := serverLifecycle(context.Background(), "teardown", []string{"--yes", "--purge", "home", "root@home"}, &out, &diagnostic, deps); code != 1 {
			t.Fatalf("code=%d", code)
		}
		out.Reset()
		diagnostic.Reset()
		if code := serverLifecycle(context.Background(), "status", []string{"--json", "home", "root@home"}, &out, &diagnostic, deps); code != 0 {
			t.Fatalf("code=%d: %s", code, &diagnostic)
		}
		var got ssh.Status
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Active != status.Active {
			t.Fatalf("status=%s err=%v", &out, err)
		}
	}
}

func TestDiscoverDeployKeyFallsBackToPrivateKeyWithoutPubFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "missing.sock"))
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if _, err := process.Run(context.Background(), "ssh-keygen", []string{"-q", "-t", "ed25519", "-N", "", "-C", "deploy@test", "-f", keyPath}, nil); err != nil {
		t.Skipf("ssh-keygen unavailable: %v", err)
	}
	if err := os.Remove(keyPath + ".pub"); err != nil {
		t.Fatal(err)
	}
	got, err := discoverDeployKey()
	if err != nil {
		t.Fatalf("discoverDeployKey failed without .pub file: %v", err)
	}
	if !strings.HasPrefix(got, "ssh-ed25519 ") || !strings.HasSuffix(got, " deploy@test") {
		t.Fatalf("unexpected discovered key %q", got)
	}
}
