package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
)

func TestExecPreservesArgumentsAndUsesStreamingOnly(t *testing.T) {
	path, _, deps := appFixture(t)
	binding := serviceBinding{HostIdentity: strings.Repeat("a", 32), Incarnation: strings.Repeat("b", 32)}
	if err := writeServiceBindings(filepath.Dir(path), "root@home", "hello", serviceBindings{"web": binding}); err != nil {
		t.Fatal(err)
	}
	command := []string{"printf", "", "two words", "$(touch /tmp/not-run)", "'\";", "café", "line\nbreak", "--"}
	input := strings.NewReader("stdin")
	deps.Stream = func(ctx context.Context, target string, r deploy.SessionRequest, binary []byte, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("default exec session inherited a metadata timeout")
		}
		if target != "root@home" || string(binary) != "binary" || !reflect.DeepEqual(r.Command, command) || r.Action != "exec" || r.Service != "web" || !r.Stdin || r.TTY || stdin != input {
			t.Fatalf("unexpected session target=%s request=%+v", target, r)
		}
		if r.Bindings["web"].Incarnation != binding.Incarnation || r.Bindings["web"].HostIdentity != binding.HostIdentity {
			t.Fatal("lost bound identity")
		}
		io.WriteString(stdout, "workload")
		io.WriteString(stderr, "diagnostic")
		return 42, nil
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{"-f", path, "--service", "web", "-i", "--"}, command...)
	code := sessionCommand(context.Background(), "exec", args, input, &stdout, &stderr, deps, terminalStreams)
	if code != 42 || stdout.String() != "workload" || !strings.Contains(stderr.String(), "diagnostic") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

func TestConsoleSelectsOnlyExplicitShellAndRequiresTerminal(t *testing.T) {
	path, _, deps := appFixture(t)
	var stdout, stderr bytes.Buffer
	args := []string{"-f", path, "--shell", "/bin/custom-shell"}
	if code := sessionCommand(context.Background(), "console", args, strings.NewReader(""), &stdout, &stderr, deps, terminalStreams); code != 2 {
		t.Fatalf("nonterminal console code=%d", code)
	}
	calls := 0
	deps.Stream = func(_ context.Context, _ string, r deploy.SessionRequest, _ []byte, _ io.Reader, _, _ io.Writer) (int, error) {
		calls++
		if !r.TTY || !r.Stdin || !reflect.DeepEqual(r.Command, []string{"/bin/custom-shell"}) {
			t.Fatalf("request=%+v", r)
		}
		return 127, nil
	}
	code := sessionCommand(context.Background(), "console", args, strings.NewReader(""), &stdout, &stderr, deps, func(io.Reader, io.Writer) bool { return true })
	if code != 127 || calls != 1 {
		t.Fatalf("console retried or lost exit status: code=%d calls=%d", code, calls)
	}
}

func TestSessionUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		action string
		args   []string
	}{
		{"exec", nil}, {"exec", []string{"--shell", "sh", "--", "ls"}},
		{"exec", []string{"-t", "--", "ls"}}, {"exec", []string{"--timeout=-1s", "--", "ls"}},
		{"exec", []string{"--app", "hello", "--", "ls"}}, {"console", []string{"-i"}},
		{"console", []string{"--", "sh"}},
	} {
		t.Run(tc.action+strings.Join(tc.args, "_"), func(t *testing.T) {
			var out bytes.Buffer
			if code := sessionCommand(context.Background(), tc.action, tc.args, nil, &out, &out, appDeps{}, terminalStreams); code != 2 {
				t.Fatalf("code=%d output=%s", code, &out)
			}
		})
	}
}

func TestFollowUsesSelectedContainerAndExplicitDeadline(t *testing.T) {
	path, _, deps := appFixture(t)
	for _, deadline := range []string{"", "2m"} {
		var out, diag bytes.Buffer
		deps.Stream = func(ctx context.Context, _ string, r deploy.SessionRequest, _ []byte, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			_, hasDeadline := ctx.Deadline()
			if hasDeadline != (deadline != "") || r.Action != "logs" || r.Service != "web" || !r.Candidate || r.Tail != 25 || stdin != nil {
				t.Fatalf("deadline=%t request=%+v stdin=%v", hasDeadline, r, stdin)
			}
			io.WriteString(stdout, "stream")
			return 0, nil
		}
		args := []string{"-f", path, "--follow", "--service", "web", "--candidate", "--tail", "25"}
		if deadline != "" {
			args = append(args, "--timeout", deadline)
		}
		if code := appCommand(context.Background(), "logs", args, &out, &diag, deps); code != 0 || out.String() != "stream" {
			t.Fatalf("code=%d out=%q diagnostic=%q", code, &out, &diag)
		}
	}
}

func TestFollowRejectsRunSnapshotsAndOtherCommands(t *testing.T) {
	for _, tc := range []struct {
		action string
		args   []string
	}{
		{"logs", []string{"--follow", "--run", strings.Repeat("a", 32)}},
		{"status", []string{"--follow=false"}}, {"logs", []string{"--follow", "--timeout=-1s"}},
	} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), tc.action, tc.args, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("code=%d output=%s", code, &out)
		}
	}
}

func TestSessionUnknownOutcomeDoesNotRetry(t *testing.T) {
	_, _, deps := appFixture(t)
	calls := 0
	deps.Stream = func(context.Context, string, deploy.SessionRequest, []byte, io.Reader, io.Writer, io.Writer) (int, error) {
		calls++
		return -1, errors.New("execution outcome unknown")
	}
	var out bytes.Buffer
	code := sessionCommand(context.Background(), "exec", []string{"--server", "home", "--app", "hello", "--", "true"}, nil, &out, &out, deps, terminalStreams)
	if code != 1 || calls != 1 || !strings.Contains(out.String(), "unknown") {
		t.Fatalf("code=%d calls=%d output=%s", code, calls, &out)
	}
}

func TestDecodeSessionRequestRejectsUnknownTrailingAndOversizeInput(t *testing.T) {
	for _, data := range []string{`{"version":1,"unknown":true}`, `{} {}`, strings.Repeat("x", 65537)} {
		if _, err := decodeSessionRequest(strings.NewReader(data)); err == nil {
			t.Fatal("accepted invalid request")
		}
	}
}
