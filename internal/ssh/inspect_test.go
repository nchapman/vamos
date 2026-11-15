package ssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/process"
)

var supportedOutput = []byte("vamos-inspect-v1\nLinux\nx86_64\n1000\nsystemd\nyes\npresent\nID=ubuntu\nVERSION_ID=24.04\n")

func TestTargets(t *testing.T) {
	for _, target := range []string{"home", "root@host.example.com", "deploy@192.0.2.1", "::1", "root@2001:db8::1", "my_host"} {
		if err := ValidateTarget(target); err != nil {
			t.Errorf("valid target %q: %v", target, err)
		}
	}
	for _, target := range []string{"", "-oProxyCommand=evil", "user@-host", "user name@host", "user@@host", "user@", "@host", "host;touch /tmp/pwn", "$(id)", "host\nother", "host:2222", "ssh://host", "[::1]", "host\x1b[31m", strings.Repeat("a", 513)} {
		t.Run(target, func(t *testing.T) {
			called := false
			_, err := inspect(context.Background(), target, func(context.Context, string, []string, []byte) (process.Result, error) {
				called = true
				return process.Result{}, nil
			})
			if err == nil || called {
				t.Fatalf("invalid target reached runner: called=%t err=%v", called, err)
			}
		})
	}
}

func TestSSHInvocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, target := range []string{"home", "deploy@192.0.2.1"} {
		t.Run(target, func(t *testing.T) {
			r, err := inspect(ctx, target, func(gotCtx context.Context, executable string, args []string, stdin []byte) (process.Result, error) {
				if executable != "ssh" || gotCtx != ctx || string(stdin) != host.Collector() {
					t.Fatal("not using direct OpenSSH with the fixed collector and caller context")
				}
				if !slices.Equal(args[len(args)-3:], []string{"--", target, "/bin/sh -s"}) {
					t.Fatalf("unsafe remote invocation: %q", args)
				}
				for _, option := range []string{"-T", "BatchMode=yes", "StrictHostKeyChecking=yes", "UpdateHostKeys=no", "ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "ControlPath=none", "RemoteCommand=none", "ConnectTimeout=10"} {
					if !slices.Contains(args, option) {
						t.Errorf("missing safety option %s", option)
					}
				}
				return process.Result{Stdout: supportedOutput}, nil
			})
			if err != nil || !r.Supported {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

func TestSSHFailurePreservesCauseAndEscapesDiagnostics(t *testing.T) {
	failure := errors.New("SSH exited 255")
	r, err := inspect(context.Background(), "home", func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: supportedOutput, Stderr: []byte("bad key\x1b[31m\n" + strings.Repeat("a", 5000))}, failure
	})
	if r != nil || !errors.Is(err, failure) || strings.ContainsRune(err.Error(), '\x1b') || !strings.Contains(err.Error(), "known_hosts") || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

func TestSSHRejectsContaminatedOutput(t *testing.T) {
	_, err := inspect(context.Background(), "home", func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: append([]byte("banner\n"), supportedOutput...)}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "framing") {
		t.Fatalf("got %v", err)
	}
}

func TestInstalledOpenSSHAcceptsOptions(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := inspect(ctx, "127.0.0.1", func(ctx context.Context, executable string, args []string, stdin []byte) (process.Result, error) {
		// -G prints effective configuration without connecting. Ignore user config
		// so this test cannot invoke their proxy commands or Match exec rules.
		args = append([]string{"-G", "-F", os.DevNull}, args...)
		result, err := process.Run(ctx, executable, args, nil)
		if err != nil {
			t.Fatalf("installed OpenSSH rejected invocation: %v; %s", err, result.Stderr)
		}
		return process.Result{Stdout: supportedOutput}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
