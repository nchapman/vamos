package buildkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/process"
)

// daemonRestart models dockerd restarting during a build with live-restore
// off: the restart=no builder is stopped and its buildx connection breaks.
func daemonRestart(f *fakeEngine) {
	f.container["State"] = map[string]any{"Running": false}
	f.network["Containers"] = map[string]any{}
}

func countCalls(f *fakeEngine, prefix string) int {
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func shortRecovery(t *testing.T, window time.Duration) {
	t.Helper()
	poll, limit := builderRecoveryPoll, builderRecoveryWindow
	builderRecoveryPoll, builderRecoveryWindow = time.Millisecond, window
	t.Cleanup(func() { builderRecoveryPoll, builderRecoveryWindow = poll, limit })
}

func TestBuildRetriesOnceWhenDaemonRestartStopsBuilder(t *testing.T) {
	m, f := newEngine(t)
	builds := 0
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "buildx" && a[1] == "build" {
			builds++
			if builds == 1 {
				daemonRestart(f)
				return process.Result{Stderr: []byte("waiting for connection")}, errors.New("exit 1"), true
			}
		}
		return process.Result{}, nil, false
	}

	r := runBuild(t, m)

	if string(r.Stdout) != "loaded image" || builds != 2 || countCalls(f, "container start") != 2 || running(f) {
		t.Fatalf("interrupted build not retried through verified startup: builds=%d %v", builds, f.calls)
	}
}

func TestBuildRetryWaitsForDaemonToReturn(t *testing.T) {
	shortRecovery(t, time.Minute)
	m, f := newEngine(t)
	builds, unavailable := 0, 0
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "buildx" && a[1] == "build" {
			builds++
			if builds == 1 {
				daemonRestart(f)
				unavailable = 3
				return process.Result{}, errors.New("exit 1"), true
			}
		}
		if unavailable > 0 && a[0] == "container" && a[1] == "inspect" {
			unavailable--
			return process.Result{}, errors.New("Cannot connect to the Docker daemon"), true
		}
		return process.Result{}, nil, false
	}

	runBuild(t, m)

	if builds != 2 || unavailable != 0 {
		t.Fatalf("build did not wait for the restarted daemon: builds=%d", builds)
	}
}

func TestBuildFailureWithRunningBuilderIsNotRetried(t *testing.T) {
	m, f := newEngine(t)
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "buildx" && a[1] == "build" {
			return process.Result{Stderr: []byte("RUN exit 17")}, errors.New("exit 1"), true
		}
		return process.Result{}, nil, false
	}

	r, err := m.Build(context.Background(), testHost, "df", "iid", nil)

	if err == nil || err.Error() != "buildkit build failed" || string(r.Stderr) != "RUN exit 17" || countCalls(f, "buildx build") != 1 {
		t.Fatalf("ordinary build failure retried or misreported: %v %v", err, f.calls)
	}
}

func TestBuildInterruptedTwiceReportsInterruption(t *testing.T) {
	m, f := newEngine(t)
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "buildx" && a[1] == "build" {
			daemonRestart(f)
			return process.Result{Stderr: []byte("second output")}, errors.New("exit 1"), true
		}
		return process.Result{}, nil, false
	}

	r, err := m.Build(context.Background(), testHost, "df", "iid", nil)

	if err == nil || !strings.Contains(err.Error(), "builder stopped during build") || string(r.Stderr) != "second output" || countCalls(f, "buildx build") != 2 {
		t.Fatalf("repeated interruption not bounded or reported: %v %v", err, f.calls)
	}
}

func TestBuildInterruptionWithUnavailableDaemonIsBounded(t *testing.T) {
	shortRecovery(t, 20*time.Millisecond)
	m, f := newEngine(t)
	down := false
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "buildx" && a[1] == "build" {
			down = true
			return process.Result{}, errors.New("exit 1"), true
		}
		if down && a[0] == "container" {
			return process.Result{}, errors.New("Cannot connect to the Docker daemon"), true
		}
		return process.Result{}, nil, false
	}

	_, err := m.Build(context.Background(), testHost, "df", "iid", nil)

	if err == nil || countCalls(f, "buildx build") != 1 {
		t.Fatalf("unavailable daemon retried or reported success: %v", err)
	}
}
