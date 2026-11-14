package main

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeExec struct {
	t         *testing.T
	mode      string
	calls     [][]string
	cancel    context.CancelFunc
	restarted bool
	prepared  bool
}

func (f *fakeExec) Run(ctx context.Context, a []string, in io.Reader, out, stderr io.Writer) error {
	f.calls = append(f.calls, append([]string(nil), a...))
	for _, s := range a {
		if s == "--" || s == "reset" || s == "--all" || s == "default" {
			f.t.Fatalf("unsafe command %v", a)
		}
	}
	if a[0] == "run" || a[0] == "delete" {
		if !strings.Contains(strings.Join(a, " "), "owned-id") {
			f.t.Fatalf("not exact ID: %v", a)
		}
	}
	switch a[0] {
	case "list":
		if f.mode == "existing" {
			io.WriteString(out, `[{"name":"soar-test-0123456789ab"}]`)
		} else {
			io.WriteString(out, `[]`)
		}
	case "create":
		if f.mode == "createfail" {
			return errors.New("create failed")
		}
	case "restart":
		if f.mode == "hang-restart" {
			<-ctx.Done()
			return ctx.Err()
		}
		if f.mode == "deadline-restart" {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("restart has no deadline")
			}
		}
		if len(a) != 2 || a[1] != "owned-id" {
			f.t.Fatalf("restart must use exact ID: %v", a)
		}
		f.restarted = true
		if f.mode == "restartfail" {
			return errors.New("restart failed")
		}
	case "info":
		isolated := "true"
		if f.mode == "isolation" {
			isolated = "false"
		}
		id := "owned-id"
		if f.mode == "replacement" && len(f.calls) > 4 {
			id = "replacement"
		}
		name, network, forward := "soar-test-0123456789ab", "true", "false"
		if (f.restarted && strings.HasPrefix(f.mode, "post-")) || (f.prepared && strings.HasPrefix(f.mode, "pre-")) {
			switch strings.TrimPrefix(strings.TrimPrefix(f.mode, "post-"), "pre-") {
			case "id":
				id = "replacement"
			case "name":
				name = "replacement"
			case "isolated":
				isolated = "false"
			case "network":
				network = "false"
			case "agent":
				forward = "true"
			}
		}
		io.WriteString(out, `{"record":{"id":"`+id+`","name":"`+name+`","config":{"isolated":`+isolated+`,"isolate_network":`+network+`,"forward_ssh_agent":`+forward+`}}}`)
	case "run":
		if a[5] == "python3" && strings.HasSuffix(a[6], "/repository/acceptance.py") {
			if !f.restarted || len(a) != 9 {
				f.t.Fatal("repository acceptance must follow restart verification")
			}
			if _, ok := ctx.Deadline(); !ok {
				f.t.Fatal("repository acceptance lacks deadline")
			}
			if f.mode == "repositoryfail" {
				return errors.New("repository acceptance failed")
			}
			return nil
		}
		if a[5] == "python3" {
			verify := len(a) == 11 && a[9] == "--phase" && a[10] == "verify-restart"
			if !verify && len(a) != 9 {
				f.t.Fatalf("unexpected suite API: %v", a)
			}
			if verify && f.mode == "hang-verify" {
				<-ctx.Done()
				return ctx.Err()
			}
			if verify && f.mode == "deadline-verify" {
				if _, ok := ctx.Deadline(); !ok {
					return errors.New("verification has no deadline")
				}
			}
			if verify && f.mode == "verifyfail" {
				return errors.New("verify failed")
			}
			if !verify {
				f.prepared = true
			}
			if f.mode == "cancel" {
				f.cancel()
				return ctx.Err()
			}
			if f.mode == "suitefail" {
				return errors.New("suite failed")
			}
		}
		if a[5] == "sh" && strings.Contains(strings.Join(a, " "), "--files-from") && ctx.Err() != nil {
			f.t.Fatal("cleanup inherited cancellation")
		}
	case "delete":
		if ctx.Err() != nil {
			f.t.Fatal("delete inherited cancellation")
		}
		if f.mode == "deletefail" {
			return errors.New("delete failed")
		}
	}
	return nil
}
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"cmd/main.go", "internal/x.go", "integration/linux_suite.py", "examples/ledger/bin/run", "docs/backup-plans.md", "docs/backups.md", "go.mod", "go.sum", "mise.toml"} {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		mode                   string
		keep, deleted, wantErr bool
	}{{"success", false, true, false}, {"repositoryfail", false, true, true}, {"repositoryfail", true, false, true}, {"suitefail", false, true, true}, {"suitefail", true, false, true}, {"cancel", false, true, true}, {"createfail", false, false, true}, {"existing", false, false, true}, {"isolation", false, true, true}, {"replacement", false, false, true}, {"deletefail", false, true, true}} {
		t.Run(tc.mode+map[bool]string{true: "keep"}[tc.keep], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fakeExec{t: t, mode: tc.mode, cancel: cancel}
			r := runner{exec: f, out: io.Discard, stderr: io.Discard}
			err := r.run(ctx, options{root: fixture(t), artifacts: t.TempDir(), name: "soar-test-0123456789ab", token: strings.Repeat("a", 32), arch: "native", ubuntu: "24.04", keep: tc.keep})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			deleted := false
			for _, a := range f.calls {
				if a[0] == "delete" {
					deleted = true
				}
				if tc.mode == "isolation" && a[0] == "run" && a[5] == "python3" {
					t.Fatal("executed suite without isolation")
				}
			}
			if deleted != tc.deleted {
				t.Fatalf("deleted=%v calls=%v", deleted, f.calls)
			}
		})
	}
}
func TestRestartPhasesHaveDeadlines(t *testing.T) {
	for _, mode := range []string{"deadline-restart", "deadline-verify"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeExec{t: t, mode: mode}
			r := runner{exec: f, out: io.Discard, stderr: io.Discard}
			err := r.run(context.Background(), options{root: fixture(t), artifacts: t.TempDir(), name: "soar-test-0123456789ab", token: strings.Repeat("a", 32), arch: "native", ubuntu: "24.04"})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartTimeoutStillDeletesOwnedMachine(t *testing.T) {
	for _, mode := range []string{"hang-restart", "hang-verify"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeExec{t: t, mode: mode}
			r := runner{exec: f, out: io.Discard, stderr: io.Discard, restartTimeout: 5 * time.Millisecond, verifyTimeout: 5 * time.Millisecond}
			err := r.run(context.Background(), options{root: fixture(t), artifacts: t.TempDir(), name: "soar-test-0123456789ab", token: strings.Repeat("a", 32), arch: "native", ubuntu: "24.04"})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected phase timeout, got %v", err)
			}
			deleted := false
			for _, args := range f.calls {
				deleted = deleted || args[0] == "delete"
			}
			if !deleted {
				t.Fatal("phase timeout skipped owned-machine cleanup")
			}
		})
	}
}

func TestControlEnvironmentDoesNotForwardAmbientVariables(t *testing.T) {
	env := controlEnvironment([]string{"PATH=/bin", "ORBENV=AWS_SECRET_ACCESS_KEY:HOME", "HOME=/Users/test"})
	if strings.Join(env, "\n") != "PATH=/bin\nHOME=/Users/test" {
		t.Fatalf("unexpected control environment: %v", env)
	}
}

func TestArchive(t *testing.T) {
	root := fixture(t)
	for _, p := range []string{".env", "bin/secret", ".pi/secret", "examples/.env.secret", "internal/__pycache__/secret", "internal/.git/secret", "integration/visual/node_modules/dependency/secret"} {
		p = filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte("secret"), 0600)
	}
	dest := filepath.Join(t.TempDir(), "source.tar")
	if err := archiveSource(root, dest); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	script := false
	docs := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "secret") || strings.Contains(h.Name, ".env") {
			t.Fatalf("secret included %s", h.Name)
		}
		if h.Name == "docs/backup-plans.md" || h.Name == "docs/backups.md" {
			docs[h.Name] = true
		}
		if h.Name == "examples/ledger/bin/run" {
			script = true
		}
	}
	if len(docs) != 2 {
		t.Fatal("documentation example tests require both backup guides in the snapshot")
	}
	if !script {
		t.Fatal("nested bin script missing")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "internal", "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privileges unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := archiveSource(root, filepath.Join(t.TempDir(), "bad.tar")); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestRestartPhases(t *testing.T) {
	for _, mode := range []string{"success", "suitefail", "restartfail", "verifyfail", "pre-id", "pre-name", "pre-isolated", "pre-network", "pre-agent", "post-id", "post-name", "post-isolated", "post-network", "post-agent"} {
		for _, keep := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "-keep"}[keep], func(t *testing.T) {
				f := &fakeExec{t: t, mode: mode}
				r := runner{exec: f, out: io.Discard, stderr: io.Discard}
				err := r.run(context.Background(), options{root: fixture(t), artifacts: t.TempDir(), name: "soar-test-0123456789ab", token: strings.Repeat("a", 32), arch: "native", ubuntu: "24.04", keep: keep})
				if (err != nil) != (mode != "success") {
					t.Fatalf("error=%v", err)
				}
				var phases []string
				deleted, collected, repository := false, false, false
				for i, a := range f.calls {
					if a[0] == "run" && strings.HasSuffix(a[6], "/repository/acceptance.py") {
						repository = true
						if strings.Join(phases, ",") != "prepare,restart,verify" || i == 0 || f.calls[i-1][0] != "info" {
							t.Fatal("repository phase ran before recovery or without fresh ownership check")
						}
					}
					if a[0] == "restart" {
						phases = append(phases, "restart")
						if i == 0 || f.calls[i-1][0] != "info" || f.calls[i-1][3] != "owned-id" {
							t.Fatal("missing pre-restart identity check")
						}
					}
					if a[0] == "run" && a[5] == "python3" && !strings.HasSuffix(a[6], "/repository/acceptance.py") {
						phase := "prepare"
						if len(a) == 11 {
							phase = "verify"
							if i < 2 || f.calls[i-1][0] != "info" || f.calls[i-1][3] != "owned-id" || f.calls[i-2][0] != "restart" {
								t.Fatal("missing post-restart identity check")
							}
						}
						phases = append(phases, phase)
					}
					if a[0] == "delete" {
						deleted = true
					}
					if a[0] == "run" && strings.Contains(strings.Join(a, " "), "--files-from") {
						collected = true
					}
				}
				want := "prepare,restart,verify"
				if mode == "suitefail" || strings.HasPrefix(mode, "pre-") {
					want = "prepare"
				} else if mode == "restartfail" || strings.HasPrefix(mode, "post-") {
					want = "prepare,restart"
				}
				if strings.Join(phases, ",") != want {
					t.Fatalf("phases=%v want %s", phases, want)
				}
				if repository != (mode == "success") {
					t.Fatalf("repository phase=%v for %s", repository, mode)
				}
				identityDrift := strings.HasSuffix(mode, "-id") || strings.HasSuffix(mode, "-name")
				if deleted != (!identityDrift && (!keep || mode == "success")) {
					t.Fatalf("deleted=%v", deleted)
				}
				if collected != !identityDrift {
					t.Fatalf("collected=%v", collected)
				}
			})
		}
	}
}
