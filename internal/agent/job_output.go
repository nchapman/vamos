package agent

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/nchapman/vamos/internal/jobresults"
)

// Keep draining both pipes even after the limit. os/exec sends the same
// comparable stdout/stderr writer to a single pipe; no inter-writer race.
type outputTail struct {
	data       [jobresults.MaxOutput]byte
	next, used int
	truncated  bool
}

func (t *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	size := len(t.data)
	if n > size-t.used {
		t.truncated = true
	}
	if n >= size {
		copy(t.data[:], p[n-size:])
		t.used = size
		t.next = 0
		return n, nil
	}
	first := copy(t.data[t.next:], p)
	copy(t.data[:], p[first:])
	t.next = (t.next + n) % size
	t.used = min(t.used+n, size)
	return n, nil
}
func (t *outputTail) bytes() []byte {
	if t.used < len(t.data) {
		return t.data[:t.used]
	}
	return append(append(make([]byte, 0, t.used), t.data[t.next:]...), t.data[:t.next]...)
}

func observeCommand(ctx context.Context, cmd *exec.Cmd, run *jobresults.Run, redact func(string) string) error {
	var tail outputTail
	cmd.Stdout, cmd.Stderr = &tail, &tail
	err := cmd.Run()
	now := time.Now()
	run.Finished = &now
	run.Output = []byte(redact(string(tail.bytes())))
	run.Truncated = tail.truncated
	if len(run.Output) > jobresults.MaxOutput {
		run.Output = run.Output[len(run.Output)-jobresults.MaxOutput:]
		run.Truncated = true
	}
	switch {
	case ctx.Err() != nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() < 0):
		run.Outcome, run.Reason = "unknown", "worker interrupted; container completion not confirmed"
	case cmd.ProcessState == nil:
		run.Outcome, run.Reason = "failed", "could not start Docker client"
	default:
		code := cmd.ProcessState.ExitCode()
		run.ExitCode = &code
		run.Outcome, run.Reason = "succeeded", ""
		if err != nil {
			run.Outcome = "failed"
		}
	}
	// Never propagate app output to the root systemd journal.
	if err != nil {
		return errors.New("scheduled command did not complete successfully")
	}
	return nil
}
