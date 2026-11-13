//go:build !windows

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func TestPreparationPublishesOnlyAfterAdmission(t *testing.T) {
	f := setup(t)
	f.m.Resolve = func(ctx context.Context, req deploy.SessionRequest) (deploy.SessionTarget, error) {
		if _, err := os.Stat(filepath.Join(f.m.Root, testToken, "executor")); err != nil {
			t.Fatalf("executor not staged: %v", err)
		}
		if _, err := os.Stat(filepath.Join(f.m.Root, testToken, "prepared.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("published before admission")
		}
		// Another operation cannot race publication while the root lock is held.
		if err := f.m.Cleanup(testToken); !errors.Is(err, errLocked) {
			t.Fatalf("preparation not locked: %v", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("preparation admission has no deadline")
		}
		return f.target, nil
	}
	f.prepare(t, testToken)
}

func TestCancellationDuringStreamCleansUp(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.m.Run = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		cancel()
		<-ctx.Done()
		return -1, ctx.Err()
	}
	code, err := f.m.Attach(ctx, testToken, nil, io.Discard, io.Discard)
	if code != -1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %d %v", code, err)
	}
	f.gone(t, testToken)
}

func TestActiveSessionCountsTowardCapacity(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	f.m.Run = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		close(started)
		<-release
		return 0, nil
	}
	go func() {
		_, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard)
		done <- err
	}()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	f.now = f.now.Add(2 * preparationTTL)
	for i := 0; i < maxSessions-1; i++ {
		f.prepare(t, fmt.Sprintf("%032x", i))
	}
	if _, err := f.m.Prepare(context.Background(), fmt.Sprintf("%032x", maxSessions), f.req, f.exe); err == nil {
		t.Fatal("active session not counted against capacity")
	}
}
