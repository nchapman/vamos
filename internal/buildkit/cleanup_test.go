package buildkit

import (
	"context"
	"errors"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestCanceledEntryStillStopsCapturedDaemon(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	f.container["State"] = map[string]any{"Running": true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Build(ctx, testHost, "df", "iid", nil); err == nil {
		t.Fatal("canceled build succeeded")
	}
	if running(f) {
		t.Fatal("canceled inventory prevented independent cleanup")
	}
}
func TestFailedInventoryStillStopsCapturedDaemon(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	f.container["State"] = map[string]any{"Running": true}
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if a[0] == "container" && a[1] == "ls" {
			return process.Result{}, errors.New("inventory unavailable"), true
		}
		return process.Result{}, nil, false
	}
	failBuild(t, m, f)
	if running(f) {
		t.Fatal("inventory failure prevented exact-ID cleanup")
	}
}
