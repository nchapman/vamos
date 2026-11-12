// Package process runs bounded, cancellable subprocesses without a local shell.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const OutputLimit = 64 * 1024

var ErrOutputLimit = errors.New("process output exceeded the configured per-stream limit")

type Result struct {
	Stdout []byte
	Stderr []byte
}

// Run caps both streams and cancels on overflow. WaitDelay also bounds waiting
// for inherited pipe handles after the command exits or its context is cancelled.
// It does not promise to terminate arbitrary proxy/remote descendants.
func Run(ctx context.Context, executable string, args []string, stdin []byte) (Result, error) {
	return run(ctx, executable, args, stdin, false, OutputLimit)
}

// RunTail is for verbose package operations: it drains output and retains a
// bounded diagnostic tail, rather than killing an installer on output overflow.
func RunTail(ctx context.Context, executable string, args []string, stdin []byte) (Result, error) {
	return run(ctx, executable, args, stdin, true, OutputLimit)
}

// RunLimited permits a larger, still explicitly bounded internal response.
// Transport protocols continue to use Run's smaller default allowance.
func RunLimited(ctx context.Context, executable string, args []string, stdin []byte, limit int) (Result, error) {
	if limit < 1 || limit > 8<<20 {
		return Result{}, errors.New("invalid process output limit")
	}
	return run(ctx, executable, args, stdin, false, limit)
}

func run(ctx context.Context, executable string, args []string, stdin []byte, tail bool, limit int) (Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &boundedBuffer{cancel: cancel, tail: tail, limit: limit}
	stderr := &boundedBuffer{cancel: cancel, tail: tail, limit: limit}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	result := Result{Stdout: stdout.buffer.Bytes(), Stderr: stderr.buffer.Bytes()}
	if stdout.exceeded || stderr.exceeded {
		return result, ErrOutputLimit
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, fmt.Errorf("run %s: %w", executable, err)
	}
	return result, nil
}

type boundedBuffer struct {
	limit    int
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
	tail     bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.tail {
		if n >= b.limit {
			b.buffer.Reset()
			b.buffer.Write(p[n-b.limit:])
		} else {
			if excess := b.buffer.Len() + n - b.limit; excess > 0 {
				b.buffer.Next(excess)
			}
			b.buffer.Write(p)
		}
		return n, nil
	}
	remaining := b.limit - b.buffer.Len()
	if n > remaining {
		b.buffer.Write(p[:remaining])
		b.exceeded = true
		b.cancel()
	} else {
		b.buffer.Write(p)
	}
	// Drain rather than returning a short write that can leave a child blocked.
	return n, nil
}
