package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/process"
)

// Keep the production runner's context-aware signature checked at compile time.
var _ Runner = RunCommand

func TestAPTMutationLockPolicy(t *testing.T) {
	for _, operation := range []string{"install", "purge"} {
		args := aptMutationArgs(operation, "docker.io")
		want := []string{"-y", "-qq", "-o", "Dpkg::Use-Pty=0", "-o", "DPkg::Lock::Timeout=60", operation, "docker.io"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %q", args)
		}
	}
}

func TestAPTLockFailureDiagnostics(t *testing.T) {
	sentinel := errors.New("exit status 100")
	for _, tc := range []struct {
		name         string
		args         []string
		output, want string
	}{
		{"install", aptMutationArgs("install", "docker.io"), "E: Unable to acquire the dpkg frontend lock", "limited to 60 seconds"},
		{"purge", aptMutationArgs("purge", "docker.io"), "E: Could not get lock /var/lib/dpkg/lock-frontend", "limited to 60 seconds"},
		{"update", []string{"update", "-qq"}, "E: Unable to lock directory /var/lib/apt/lists/", "automatic waiting for this lock is not implemented"},
		{"simulation", []string{"--simulate", "install", "docker.io"}, "E: Could not get lock", "APT could not acquire"},
		{"other", aptMutationArgs("install", "docker.io"), "E: Unable to locate package", "apt-get failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(context.Background(), struct{}{}, "caller")
			m := Manager{Run: func(got context.Context, name string, args []string) (process.Result, error) {
				calls++
				if got != ctx || name != "apt-get" || !reflect.DeepEqual(args, tc.args) {
					t.Fatal("runner invocation changed")
				}
				return process.Result{Stderr: []byte(tc.output)}, sentinel
			}}
			_, err := m.command(ctx, "apt-get", tc.args...)
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), tc.want) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if tc.name != "other" && !strings.Contains(err.Error(), "do not delete lock files or kill package processes") {
				t.Fatal(err)
			}
		})
	}
}

func TestRunCommandAPTSafety(t *testing.T) {
	dir := t.TempDir()
	// A private PATH executable ensures no real package manager is invoked.
	script := "#!/bin/sh\ncase \"$1\" in\nwait) while :; do :; done ;;\n*) i=0; while [ $i -lt 12000 ]; do printf 'package progress line\\n'; i=$((i+1)); done ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "apt-get"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Run("mutation drains bounded tail", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := RunCommand(ctx, "apt-get", aptMutationArgs("install", "docker.io"))
		if err != nil || len(result.Stdout) != process.OutputLimit {
			t.Fatalf("bytes=%d err=%v", len(result.Stdout), err)
		}
	})
	t.Run("simulation retains overflow protection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := RunCommand(ctx, "apt-get", []string{"--simulate", "install", "docker.io"})
		if !errors.Is(err, process.ErrOutputLimit) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("caller deadline wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := RunCommand(ctx, "apt-get", []string{"wait", "-o", aptLockTimeoutOption})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
			t.Fatalf("elapsed=%v err=%v", time.Since(start), err)
		}
	})
}
