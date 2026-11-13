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
)

type fakeExec struct {
	t      *testing.T
	mode   string
	calls  [][]string
	cancel context.CancelFunc
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
	case "info":
		isolated := "true"
		if f.mode == "isolation" {
			isolated = "false"
		}
		id := "owned-id"
		if f.mode == "replacement" && len(f.calls) > 4 {
			id = "replacement"
		}
		io.WriteString(out, `{"record":{"id":"`+id+`","name":"soar-test-0123456789ab","config":{"isolated":`+isolated+`,"isolate_network":true,"forward_ssh_agent":false}}}`)
	case "run":
		if a[5] == "python3" {
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
	for _, p := range []string{"cmd/main.go", "internal/x.go", "integration/linux_suite.py", "examples/ledger/bin/run", "go.mod", "go.sum", "mise.toml"} {
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
	}{{"success", false, true, false}, {"suitefail", false, true, true}, {"suitefail", true, false, true}, {"cancel", false, true, true}, {"createfail", false, false, true}, {"existing", false, false, true}, {"isolation", false, true, true}, {"replacement", false, false, true}, {"deletefail", false, true, true}} {
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
func TestControlEnvironmentDoesNotForwardAmbientVariables(t *testing.T) {
	env := controlEnvironment([]string{"PATH=/bin", "ORBENV=AWS_SECRET_ACCESS_KEY:HOME", "HOME=/Users/test"})
	if strings.Join(env, "\n") != "PATH=/bin\nHOME=/Users/test" {
		t.Fatalf("unexpected control environment: %v", env)
	}
}

func TestArchive(t *testing.T) {
	root := fixture(t)
	for _, p := range []string{".env", "bin/secret", ".pi/secret", "examples/.env.secret", "internal/__pycache__/secret", "internal/.git/secret"} {
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
		if h.Name == "examples/ledger/bin/run" {
			script = true
		}
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
