package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/jobresults"
)

func TestJobOutputFixture(t *testing.T) {
	if os.Getenv("VAMOS_JOB_FAILURE_FIXTURE") == "1" {
		fmt.Fprint(os.Stdout, strings.Repeat("x", jobresults.MaxOutput)+"stdout-marker\n")
		fmt.Fprintln(os.Stderr, "stderr-marker sensitive")
		os.Exit(7)
	}
}

func TestObserveFailedJobRetainsCombinedRedactedTail(t *testing.T) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobOutputFixture$")
	cmd.Env = append(os.Environ(), "VAMOS_JOB_FAILURE_FIXTURE=1")
	var run jobresults.Run
	err := observeCommand(ctx, cmd, &run, func(s string) string { return strings.ReplaceAll(s, "sensitive", "[REDACTED]") })
	if err == nil || run.Outcome != "failed" || run.ExitCode == nil || *run.ExitCode != 7 || !run.Truncated || len(run.Output) > jobresults.MaxOutput || !bytes.HasSuffix(run.Output, []byte("stdout-marker\nstderr-marker [REDACTED]\n")) {
		t.Fatalf("outcome=%s bytes=%d error=%v", run.Outcome, len(run.Output), err)
	}
}

func TestJobLaunchRefusalDistinguishesLostAcknowledgement(t *testing.T) {
	const unit = "vamos-job-app.web.cleanup.service"
	for _, tc := range []struct {
		started          bool
		diagnostic, want string
	}{
		{false, "", "could not start systemd-run"},
		{true, "Failed to connect to bus: No such file or directory", "could not connect to systemd"},
		{true, "Failed to start transient service unit: Unit " + unit + " was already loaded or has a fragment file.", "previous job unit still active or being collected"},
		{true, "connection reset", ""},
	} {
		if got := launchRefusal(tc.started, unit, tc.diagnostic); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.diagnostic, got, tc.want)
		}
	}
}

func TestOutputTailAcrossWriteBoundaries(t *testing.T) {
	var tail outputTail
	var full []byte
	for _, p := range [][]byte{[]byte("hello"), bytes.Repeat([]byte{'x'}, jobresults.MaxOutput), bytes.Repeat([]byte{'y'}, 1200), bytes.Repeat([]byte{'z'}, jobresults.MaxOutput-1), []byte("last")} {
		full = append(full, p...)
		n, err := tail.Write(p)
		if n != len(p) || err != nil || !bytes.Equal(tail.bytes(), full[max(0, len(full)-jobresults.MaxOutput):]) || tail.truncated != (len(full) > jobresults.MaxOutput) {
			t.Fatal("incorrect retained tail")
		}
	}
}
