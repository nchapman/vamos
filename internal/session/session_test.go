//go:build !windows

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fixture struct {
	m      *Manager
	req    deploy.SessionRequest
	target deploy.SessionTarget
	exe    string
	now    time.Time
	runs   int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Now().UTC()}
	f.req = deploy.SessionRequest{Version: 1, Action: "exec", Host: lifecycle.Config{Name: "host", Domain: "example.com"}, App: "app", Command: []string{"echo", "$(touch /tmp/not-a-shell); *", "", "a b", "--flag"}}
	f.target = deploy.SessionTarget{HostIdentity: "host-id", App: "app", Service: "web", Incarnation: "incarnation", Operation: "operation", ContainerID: strings.Repeat("a", 64), Container: "display-only"}
	f.exe = filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(f.exe, []byte("test executable bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	f.m = &Manager{Root: filepath.Join(t.TempDir(), "sessions"), Now: func() time.Time { return f.now },
		Resolve: func(context.Context, deploy.SessionRequest) (deploy.SessionTarget, error) { return f.target, nil },
		Run: func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
			f.runs++
			return 0, nil
		}}
	return f
}

func (f *fixture) prepare(t *testing.T, token string) Preparation {
	t.Helper()
	p, err := f.m.Prepare(context.Background(), token, f.req, f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) gone(t *testing.T, token string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(f.m.Root, token)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging remains: %v", err)
	}
}

func TestPrepareAttachLiteralStreamingAndSingleUse(t *testing.T) {
	f := setup(t)
	p := f.prepare(t, testToken)
	if p.Version != 1 || p.Token != testToken || p.Target != f.target || p.Expires != f.now.Add(2*time.Minute) || p.Error != "" {
		t.Fatalf("preparation: %+v", p)
	}
	copied, err := os.ReadFile(filepath.Join(f.m.Root, testToken, "executor"))
	if err != nil || string(copied) != "test executable bytes" {
		t.Fatalf("executor: %q, %v", copied, err)
	}
	// The staged executable and captured request do not depend on original inputs.
	if err := os.Remove(f.exe); err != nil {
		t.Fatal(err)
	}
	command := append([]string(nil), f.req.Command...)
	f.req.Command[0] = "mutated"
	var out, diagnostic bytes.Buffer
	payload := strings.Repeat("raw\x00\xff\n", 20000)
	f.m.Run = func(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		f.runs++
		expected := append([]string{"exec", f.target.ContainerID}, command...)
		if name != "docker" || !reflect.DeepEqual(args, expected) {
			t.Fatalf("invocation: %q %q", name, args)
		}
		if stdin != nil {
			t.Fatal("stdin was not opted in")
		}
		if stdout != &out || stderr != &diagnostic {
			t.Fatal("streams wrapped or replaced")
		}
		if _, err := os.Stat(filepath.Join(f.m.Root, testToken, "executor")); err != nil {
			t.Fatal("executor removed before attachment")
		}
		io.WriteString(stdout, payload)
		io.WriteString(stderr, "raw stderr\x00\xff")
		return 37, nil
	}
	code, err := f.m.Attach(context.Background(), testToken, strings.NewReader("ignored"), &out, &diagnostic)
	if code != 37 || err != nil || out.String() != payload || diagnostic.String() != "raw stderr\x00\xff" {
		t.Fatalf("result: %d %v", code, err)
	}
	f.gone(t, testToken)
	if code, err = f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard); code != -1 || err == nil {
		t.Fatalf("replay: %d %v", code, err)
	}
	if f.runs != 1 {
		t.Fatalf("runs: %d", f.runs)
	}
}

func TestStdinTTYAndLogsArguments(t *testing.T) {
	for _, action := range []string{"stdin", "tty", "logs"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			var want []string
			input := strings.NewReader("input")
			switch action {
			case "stdin", "tty":
				f.req.Stdin = true
				f.req.TTY = action == "tty"
				want = []string{"exec", "-i"}
				if f.req.TTY {
					want = append(want, "-t")
				}
				want = append(want, f.target.ContainerID)
				want = append(want, f.req.Command...)
			case "logs":
				f.req.Action, f.req.Command, f.req.Tail, f.req.Candidate = "logs", nil, 29, true
				f.target.Candidate = true
				want = []string{"logs", "--timestamps", "--tail", "29", "--follow", f.target.ContainerID}
			}
			f.prepare(t, testToken)
			f.m.Run = func(_ context.Context, name string, args []string, stdin io.Reader, _, _ io.Writer) (int, error) {
				if name != "docker" || !reflect.DeepEqual(args, want) {
					t.Fatalf("argv: %q", args)
				}
				if action == "logs" {
					if stdin != nil {
						t.Fatal("logs stdin")
					}
				} else if stdin != input {
					t.Fatal("stdin not passed literally")
				}
				return 0, nil
			}
			if _, err := f.m.Attach(context.Background(), testToken, input, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			f.gone(t, testToken)
		})
	}
}

func TestEveryTargetFieldPinned(t *testing.T) {
	typ := reflect.TypeOf(deploy.SessionTarget{})
	for i := 0; i < typ.NumField(); i++ {
		t.Run(typ.Field(i).Name, func(t *testing.T) {
			f := setup(t)
			f.prepare(t, testToken)
			field := reflect.ValueOf(&f.target).Elem().Field(i)
			if field.Kind() == reflect.Bool {
				field.SetBool(!field.Bool())
			} else {
				field.SetString(field.String() + "changed")
			}
			code, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard)
			if code != -1 || err == nil || !strings.Contains(err.Error(), "target changed") || f.runs != 0 {
				t.Fatalf("changed target: %d %v", code, err)
			}
			f.gone(t, testToken)
		})
	}
}

func TestExpiryAndFailuresConsumeStaging(t *testing.T) {
	for _, failure := range []string{"expiry", "resolve", "run", "cancel", "malformed", "oversize", "claimed"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t)
			f.prepare(t, testToken)
			ctx := context.Background()
			sentinel := errors.New("injected failure")
			switch failure {
			case "expiry":
				f.now = f.now.Add(preparationTTL)
			case "resolve":
				f.m.Resolve = func(context.Context, deploy.SessionRequest) (deploy.SessionTarget, error) {
					return deploy.SessionTarget{}, sentinel
				}
			case "run":
				f.m.Run = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
					return -1, sentinel
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "malformed", "oversize":
				data := []byte("{")
				if failure == "oversize" {
					data = bytes.Repeat([]byte(" "), maxMetadata+1)
				}
				if err := os.WriteFile(filepath.Join(f.m.Root, testToken, "prepared.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "claimed":
				if err := os.Rename(filepath.Join(f.m.Root, testToken, "prepared.json"), filepath.Join(f.m.Root, testToken, "claimed.json")); err != nil {
					t.Fatal(err)
				}
			}
			code, err := f.m.Attach(ctx, testToken, nil, io.Discard, io.Discard)
			if code != -1 || err == nil {
				t.Fatalf("failure: %d %v", code, err)
			}
			if (failure == "resolve" || failure == "run") && !errors.Is(err, sentinel) {
				t.Fatalf("lost error: %v", err)
			}
			if f.runs != 0 {
				t.Fatal("unexpected invocation")
			}
			f.gone(t, testToken)
		})
	}
}

func TestPruneUnclaimedPartialAndAbandonedClaim(t *testing.T) {
	for _, kind := range []string{"prepared", "partial", "partial-no-lock", "claimed"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.prepare(t, testToken)
			dir := filepath.Join(f.m.Root, testToken)
			switch kind {
			case "partial", "partial-no-lock":
				if err := os.Rename(filepath.Join(dir, "prepared.json"), filepath.Join(dir, "record.tmp")); err != nil {
					t.Fatal(err)
				}
				if kind == "partial-no-lock" {
					if err := os.Remove(filepath.Join(dir, "lock")); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chtimes(dir, f.now, f.now); err != nil {
					t.Fatal(err)
				}
			case "claimed":
				if err := os.Rename(filepath.Join(dir, "prepared.json"), filepath.Join(dir, "claimed.json")); err != nil {
					t.Fatal(err)
				}
			}
			f.now = f.now.Add(preparationTTL)
			second := strings.Repeat("b", 32)
			f.prepare(t, second)
			f.gone(t, testToken)
			if err := f.m.Cleanup(second); err != nil {
				t.Fatal(err)
			}
			f.gone(t, second)
			if err := f.m.Cleanup(second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActiveSessionExclusiveAndNotPruned(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var runs atomic.Int32
	f.m.Run = func(_ context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		runs.Add(1)
		close(started)
		<-release
		return 0, nil
	}
	go func() {
		_, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard)
		done <- err
	}()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
		f.gone(t, testToken)
	}()
	if err := f.m.Cleanup(testToken); !errors.Is(err, errLocked) {
		t.Fatalf("active cleanup: %v", err)
	}
	if _, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard); !errors.Is(err, errLocked) {
		t.Fatalf("concurrent attach: %v", err)
	}
	f.now = f.now.Add(24 * time.Hour)
	second := strings.Repeat("b", 32)
	f.prepare(t, second)
	if _, err := os.Stat(filepath.Join(f.m.Root, testToken, "executor")); err != nil {
		t.Fatalf("active session pruned: %v", err)
	}
	if runs.Load() != 1 {
		t.Fatal("concurrent invocation")
	}
}

func TestCapacityAndDuplicatePreparation(t *testing.T) {
	f := setup(t)
	for i := 0; i < maxSessions; i++ {
		f.prepare(t, fmt.Sprintf("%032x", i))
	}
	if _, err := f.m.Prepare(context.Background(), testToken, f.req, f.exe); err == nil {
		t.Fatal("capacity not enforced")
	}
	f.now = f.now.Add(preparationTTL)
	f.prepare(t, testToken)
	if _, err := f.m.Prepare(context.Background(), testToken, f.req, f.exe); err == nil {
		t.Fatal("duplicate token replaced")
	}
	if _, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareFailureCleanupAndValidation(t *testing.T) {
	for _, failure := range []string{"request", "resolve", "executable", "oversize", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			switch failure {
			case "request":
				f.req.Command = nil
			case "resolve":
				f.m.Resolve = func(context.Context, deploy.SessionRequest) (deploy.SessionTarget, error) {
					return deploy.SessionTarget{}, errors.New("not admitted")
				}
			case "executable":
				f.exe += ".missing"
			case "oversize":
				if err := os.Truncate(f.exe, maxExecutable+1); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			p, err := f.m.Prepare(ctx, testToken, f.req, f.exe)
			if err == nil || p.Error == "" {
				t.Fatalf("failure not reported: %+v %v", p, err)
			}
			f.gone(t, testToken)
		})
	}
	for _, token := range []string{"", "../outside", strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("z", 32)} {
		t.Run("token="+token, func(t *testing.T) {
			f := setup(t)
			if _, err := f.m.Prepare(context.Background(), token, f.req, f.exe); err == nil {
				t.Fatal("bad prepare token accepted")
			}
			if _, err := f.m.Attach(context.Background(), token, nil, nil, nil); err == nil {
				t.Fatal("bad attach token accepted")
			}
			if err := f.m.Cleanup(token); err == nil {
				t.Fatal("bad cleanup token accepted")
			}
		})
	}
}

func TestRootLockDoesNotWait(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	root, err := openRoot(f.m.Root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockFile(root, ".lock", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := f.m.Prepare(context.Background(), strings.Repeat("b", 32), f.req, f.exe); !errors.Is(err, errLocked) {
		t.Fatalf("prepare root lock: %v", err)
	}
	if _, err := f.m.Attach(context.Background(), testToken, nil, nil, nil); !errors.Is(err, errLocked) {
		t.Fatalf("attach root lock: %v", err)
	}
	if err := f.m.Cleanup(testToken); !errors.Is(err, errLocked) {
		t.Fatalf("cleanup root lock: %v", err)
	}
}

func TestAttachIndependentlyValidatesStoredRequest(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	path := filepath.Join(f.m.Root, testToken, "prepared.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Request.Action = "build"
	data, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Attach(context.Background(), testToken, nil, nil, nil); err == nil {
		t.Fatal("stored invalid request accepted")
	}
	if f.runs != 0 {
		t.Fatal("invalid request invoked")
	}
	f.gone(t, testToken)
}
