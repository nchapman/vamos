package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// Stream connects a process directly to caller-owned streams, without buffering
// or imposing the metadata RPC output limits. File descriptors are passed through
// unchanged so OpenSSH can own terminal mode and window-size handling.
// A normal nonzero exit is a result, not a transport error. Start, cancellation,
// signal and stream failures return -1. Cancellation does not establish that
// commands started inside a remote container have stopped.
func Stream(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode(), nil
	}
	return -1, err
}
