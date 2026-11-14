package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/agentproto"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
)

func testPrincipal() agentproto.Principal {
	return agentproto.Principal{ID: "ci:billing", Role: agentproto.RoleDeployer, Apps: []string{"billing"}, Actions: []string{"status"}}
}
func testSubmit(key string) agentproto.SubmitRequest {
	return agentproto.SubmitRequest{IdempotencyKey: key, Request: deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}}
}

func TestQueuePersistsDeduplicatesAndRecoversRunningWork(t *testing.T) {
	q, err := NewQueue(filepath.Join(t.TempDir(), "queue"), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	q.newID = func() (string, error) { return "0123456789abcdef0123456789abcdef", nil }
	first, err := q.Submit(context.Background(), testPrincipal(), testSubmit("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Phase != agentproto.PhaseQueued || first.QueuePosition != 1 || first.Deduplicated {
		t.Fatalf("unexpected first submit: %+v", first)
	}
	second, err := q.Submit(context.Background(), testPrincipal(), testSubmit("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated || second.OperationID != first.OperationID {
		t.Fatalf("did not deduplicate: %+v", second)
	}

	q.mu.Lock()
	ops, err := q.recordsLocked()
	if err != nil {
		q.mu.Unlock()
		t.Fatal(err)
	}
	op := ops[0]
	op.Phase = agentproto.PhaseRunning
	if err := q.saveLocked(op); err != nil {
		q.mu.Unlock()
		t.Fatal(err)
	}
	q.mu.Unlock()
	if err := q.Recover(); err != nil {
		t.Fatal(err)
	}
	record, err := q.Get(first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != agentproto.PhaseUncertain || record.Error == "" {
		t.Fatalf("restart did not fence running operation: %+v", record)
	}
}

func TestQueueStartsAndFinishesOnlyStagedOperation(t *testing.T) {
	q, err := NewQueue(filepath.Join(t.TempDir(), "queue"), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := q.Submit(context.Background(), testPrincipal(), testSubmit("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	request, payload, err := q.Start(accepted.OperationID)
	if err != nil || request.Action != "status" || len(payload) != 0 {
		t.Fatalf("start request=%+v payload=%q err=%v", request, payload, err)
	}
	if err := q.Finish(accepted.OperationID, deploy.Response{Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	record, err := q.Get(accepted.OperationID)
	if err != nil || record.Phase != agentproto.PhaseSucceeded || record.Response == nil {
		t.Fatalf("finish record=%+v err=%v", record, err)
	}
	if _, _, err := q.Start(accepted.OperationID); err == nil {
		t.Fatal("terminal operation was restarted")
	}
}

func TestQueuePersistsExactPayloadAndFencesBadTransfer(t *testing.T) {
	q, err := NewQueue(filepath.Join(t.TempDir(), "queue"), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("private-build-context")
	hash := sha256.Sum256(payload)
	request := testSubmit("dddddddddddddddddddddddddddddddd")
	request.PayloadBytes = int64(len(payload))
	request.PayloadSHA256 = hex.EncodeToString(hash[:])
	accepted, err := q.Submit(context.Background(), testPrincipal(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.StorePayload(accepted.OperationID, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(q.payloadPath(accepted.OperationID))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("stored payload=%q err=%v", data, err)
	}

	bad := testSubmit("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	bad.PayloadBytes, bad.PayloadSHA256 = 2, strings.Repeat("0", 64)
	accepted, err = q.Submit(context.Background(), testPrincipal(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.StorePayload(accepted.OperationID, strings.NewReader("no")); err == nil {
		t.Fatal("bad payload transfer was accepted")
	}
	record, err := q.Get(accepted.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != agentproto.PhaseUncertain {
		t.Fatalf("bad payload did not fence operation: %+v", record)
	}
}

func TestQueueRejectsPrincipalScopeAndIdempotencyCollision(t *testing.T) {
	q, err := NewQueue(filepath.Join(t.TempDir(), "queue"), agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Submit(context.Background(), testPrincipal(), testSubmit("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}
	different := testSubmit("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	different.Request.Service = "web"
	if _, err := q.Submit(context.Background(), testPrincipal(), different); err == nil {
		t.Fatal("different request reused an idempotency key")
	}
	if _, err := q.Get("../../../../../../../../../../../../../../"); err == nil {
		t.Fatal("path-like operation ID was accepted")
	}
	other := testPrincipal()
	other.ID = "ci:other"
	other.Apps = []string{"billing"}
	if _, err := q.Submit(context.Background(), other, testSubmit("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err == nil {
		t.Fatalf("expected idempotency collision refusal, got %v", err)
	}
	forbidden := testPrincipal()
	forbidden.Apps = []string{"other"}
	if _, err := q.Submit(context.Background(), forbidden, testSubmit("cccccccccccccccccccccccccccccccc")); err == nil {
		t.Fatal("out-of-scope operation was admitted")
	}
}
