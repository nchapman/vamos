package process_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/process"
)

func helperArgs(mode string) []string { return []string{"-test.run=^TestProcessHelper$", "--", mode} }

func TestProcessHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--" || i+1 >= len(os.Args) {
			continue
		}
		switch os.Args[i+1] {
		case "echo":
			io.Copy(os.Stdout, os.Stdin)
			fmt.Fprint(os.Stderr, "diagnostic")
		case "tail":
			os.Stdout.Write(bytes.Repeat([]byte("a"), process.OutputLimit*4))
			os.Stderr.Write(bytes.Repeat([]byte("b"), process.OutputLimit*4))
			fmt.Fprint(os.Stdout, "stdout finished")
			fmt.Fprint(os.Stderr, "stderr finished")
		case "fail":
			fmt.Fprint(os.Stdout, "not a successful response")
			fmt.Fprint(os.Stderr, "failure")
			os.Exit(23)
		case "hang":
			time.Sleep(30 * time.Second)
		case "stdout-flood":
			os.Stdout.Write(bytes.Repeat([]byte("x"), process.OutputLimit*4))
		case "stderr-flood":
			os.Stderr.Write(bytes.Repeat([]byte("x"), process.OutputLimit*4))
		case "inherit":
			cmd := exec.Command(os.Args[0], helperArgs("hold")...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				os.Exit(24)
			}
		case "hold":
			time.Sleep(2 * time.Second)
		default:
			os.Exit(25)
		}
		os.Exit(0)
	}
}

func TestRunPreservesStreamsAndInput(t *testing.T) {
	input := []byte("spaces; 'quotes'\n\x00unicode: café")
	r, err := process.Run(context.Background(), os.Args[0], helperArgs("echo"), input)
	if err != nil || !bytes.Equal(r.Stdout, input) || string(r.Stderr) != "diagnostic" {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestRunFailures(t *testing.T) {
	t.Run("nonzero exit", func(t *testing.T) {
		r, err := process.Run(context.Background(), os.Args[0], helperArgs("fail"), nil)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 23 || string(r.Stderr) != "failure" {
			t.Fatalf("result=%+v err=%v", r, err)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		_, err := process.Run(context.Background(), "vamos-test-executable-does-not-exist", nil, nil)
		if err == nil {
			t.Fatal("missing executable succeeded")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := process.Run(ctx, os.Args[0], helperArgs("hang"), nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := process.Run(ctx, os.Args[0], helperArgs("echo"), nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	for _, mode := range []string{"stdout-flood", "stderr-flood"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := process.Run(ctx, os.Args[0], helperArgs(mode), nil)
			if !errors.Is(err, process.ErrOutputLimit) || len(r.Stdout) > process.OutputLimit || len(r.Stderr) > process.OutputLimit {
				t.Fatalf("stdout=%d stderr=%d err=%v", len(r.Stdout), len(r.Stderr), err)
			}
		})
	}
	t.Run("inherited pipes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := process.Run(ctx, os.Args[0], helperArgs("inherit"), nil)
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("got %v, want bounded inherited-pipe wait", err)
		}
	})
}

func TestOutputExactlyAtLimit(t *testing.T) {
	// The diagnostic uses its own allowance, independent of stdout.
	r, err := process.Run(context.Background(), os.Args[0], helperArgs("echo"), []byte(strings.Repeat("a", process.OutputLimit)))
	if err != nil || len(r.Stdout) != process.OutputLimit {
		t.Fatalf("stdout=%d err=%v", len(r.Stdout), err)
	}
}

func TestRunTailDrainsWithoutCancelling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := process.RunTail(ctx, os.Args[0], helperArgs("tail"), nil)
	wantOut := strings.Repeat("a", process.OutputLimit-len("stdout finished")) + "stdout finished"
	wantErr := strings.Repeat("b", process.OutputLimit-len("stderr finished")) + "stderr finished"
	if err != nil || string(r.Stdout) != wantOut || string(r.Stderr) != wantErr {
		t.Fatalf("stdout=%d stderr=%d err=%v", len(r.Stdout), len(r.Stderr), err)
	}
}
