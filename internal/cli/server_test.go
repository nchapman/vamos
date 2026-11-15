package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/host"
)

func TestInspectPresentation(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			for _, root := range []bool{false, true} {
				args := []string{"--timeout", "2s", "home"}
				if asJSON {
					args = append([]string{"--json"}, args...)
				}
				var stdout, stderr bytes.Buffer
				code := inspectServer(context.Background(), args, &stdout, &stderr, func(ctx context.Context, target string) (*host.Report, error) {
					if target != "home" {
						t.Fatalf("wrong target %q", target)
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("missing deadline")
					}
					return &host.Report{OS: "ubuntu", Version: "24.04", Supported: supported, Root: root, Problems: []string{"test reason"}}, nil
				})
				wantCode := 1
				if supported {
					wantCode = 0
				}
				if code != wantCode || stderr.Len() != 0 {
					t.Fatalf("code=%d stderr=%q", code, &stderr)
				}
				if asJSON {
					var r host.Report
					if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || r.Supported != supported {
						t.Fatalf("invalid JSON report: %q", &stdout)
					}
				} else if !strings.Contains(stdout.String(), "OS: ubuntu 24.04") || (!root && !strings.Contains(stdout.String(), "sudo was not invoked")) {
					t.Fatalf("invalid text report: %q", &stdout)
				}
			}
		}
	}
}

func TestInspectUsageDoesNotConnect(t *testing.T) {
	for _, args := range [][]string{nil, {"a", "b"}, {"--timeout", "0s", "home"}, {"--timeout", "bad", "home"}, {"--unknown"}, {"home", "--json"}, {"host;bad"}} {
		var stdout, stderr bytes.Buffer
		code := inspectServer(context.Background(), args, &stdout, &stderr, func(context.Context, string) (*host.Report, error) {
			t.Fatal("usage error initiated connection")
			return nil, nil
		})
		if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, &stdout, &stderr)
		}
	}
}

func TestInspectErrorAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := inspectServer(ctx, []string{"home"}, &stdout, &stderr, func(ctx context.Context, _ string) (*host.Report, error) {
		return nil, ctx.Err()
	})
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

func TestServerAndExecutorRouting(t *testing.T) {
	for _, args := range [][]string{{"server"}, {"server", "--help"}, {"server", "inspect", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "dev"); code != 0 || !strings.Contains(stdout.String()+stderr.String(), "TARGET") {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, &stdout, &stderr)
		}
	}
	for _, args := range [][]string{{"server", "add"}, {"internal-executor", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "dev"); code != 2 {
			t.Fatalf("args=%q code=%d", args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	code := RunContext(context.Background(), []string{"internal-executor"}, strings.NewReader(`{"version":99,"operation":"inspect"}`), &stdout, &stderr, "dev")
	if code != 2 || stderr.Len() != 0 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("executor routing: code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

func TestInspectWriteFailure(t *testing.T) {
	for _, args := range [][]string{{"--json", "home"}, {"home"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			code := inspectServer(context.Background(), args, brokenWriter{}, &stderr, func(context.Context, string) (*host.Report, error) {
				return &host.Report{Supported: true}, nil
			})
			if code != 1 || !strings.Contains(stderr.String(), "write report") {
				t.Fatalf("code=%d stderr=%q", code, &stderr)
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }
