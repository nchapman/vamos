package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/deploy"
)

func TestRetryWorkerResultCannotChangeTerminalRecord(t *testing.T) {
	q, err := NewQueue(t.TempDir(), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	op, err := q.Submit(t.Context(), testPrincipal(), testSubmit(strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Start(op.OperationID); err != nil {
		t.Fatal(err)
	}
	result := deploy.Response{Version: 1}
	if err := q.Finish(op.OperationID, result, nil); err != nil {
		t.Fatal(err)
	}
	before, err := q.Get(op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(op.OperationID, result, nil); err != nil {
		t.Fatal(err)
	}
	result.Error = "different outcome"
	if err := q.Finish(op.OperationID, result, nil); err == nil {
		t.Fatal("overwrote terminal response")
	}
	after, err := q.Get(op.OperationID)
	if err != nil || after.Phase != agentproto.PhaseSucceeded || !after.FinishedAt.Equal(*before.FinishedAt) {
		t.Fatalf("terminal record changed: %+v, %v", after, err)
	}
}

func TestExitedWorkerResultSurvivesQueueFailure(t *testing.T) {
	q, err := NewQueue(t.TempDir(), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	op, err := q.Submit(t.Context(), testPrincipal(), testSubmit(strings.Repeat("a", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Start(op.OperationID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result := filepath.Join(dir, "response.json")
	if err := os.WriteFile(result, []byte(`{"version":1,"error":"worker-specific failure"}`), 0600); err != nil {
		t.Fatal(err)
	}
	oldQuery := queryWorkerUnitActive
	queryWorkerUnitActive = func(context.Context, string) (bool, error) { return false, nil }
	t.Cleanup(func() { queryWorkerUnitActive = oldQuery })

	// Make the queue unavailable while the agent observes the exited unit.
	path := q.recordPath(op.OperationID)
	if err := os.Rename(path, path+".offline"); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Queue: q}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	d.monitorSupervisedUnit(ctx, op.OperationID, dir)
	if _, err := os.Stat(result); err != nil {
		t.Fatalf("lost worker result before the queue could record it: %v", err)
	}

	queries := 0
	queryWorkerUnitActive = func(context.Context, string) (bool, error) {
		queries++
		if queries == 2 {
			if err := os.Rename(path+".offline", path); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	retryCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d.monitorSupervisedUnit(retryCtx, op.OperationID, dir)
	rec, err := q.Get(op.OperationID)
	if err != nil || rec.Response == nil || rec.Response.Error != "worker-specific failure" || rec.Phase != agentproto.PhaseFailed {
		t.Fatalf("did not recover exact worker result: %+v, %v", rec, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected cleanup after durable result, got %v", err)
	}
}
