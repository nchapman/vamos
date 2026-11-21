// Package agent implements the installed server-side coordinator. It persists
// admission decisions before workers run, so reconnecting clients can reattach
// without creating duplicate app mutations.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/executor"
)

const recordsDirectory = "operations"

var errSubmitForbidden = errors.New("submission forbidden")

// After rename the record may already be visible even if directory sync fails.
var errRecordSync = errors.New("cannot confirm operation record durability")

var operationIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

type operation struct {
	agentproto.OperationRecord
	Request       deploy.Request `json:"request"`
	PayloadBytes  int64          `json:"payload_bytes,omitempty"`
	PayloadSHA256 string         `json:"payload_sha256,omitempty"`
	PayloadReady  bool           `json:"payload_ready,omitempty"`
}

// Queue is a durable, single-process admission store. The coordinator owns one
// Queue instance; each operation is separately atomically published so a crash
// cannot turn a partially-written request into an admitted mutation.
type Queue struct {
	root          string
	now           func() time.Time
	newID         func() (string, error)
	mu            sync.Mutex
	inFlightBytes int64
	changed       chan struct{}
	policy        agentproto.ConcurrencyPolicy
}

func NewQueue(root string, policy agentproto.ConcurrencyPolicy) (*Queue, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	q := &Queue{root: root, now: time.Now, newID: randomID, changed: make(chan struct{}), policy: policy}
	if err := q.ensureRoot(); err != nil {
		return nil, err
	}
	return q, nil
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func (q *Queue) ensureRoot() error {
	for _, path := range []string{q.root, filepath.Join(q.root, recordsDirectory)} {
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("agent queue path %s must be a real directory", path)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) recordPath(id string) string {
	return filepath.Join(q.root, recordsDirectory, id+".json")
}
func (q *Queue) payloadPath(id string) string {
	return filepath.Join(q.root, recordsDirectory, id+".payload")
}

func isTempQueueEntry(name string) bool {
	return strings.HasPrefix(name, ".record-") || strings.HasPrefix(name, ".payload-")
}

func (q *Queue) recordsLocked() ([]operation, error) {
	entries, err := os.ReadDir(filepath.Join(q.root, recordsDirectory))
	if err != nil {
		return nil, err
	}
	var ops []operation
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) == ".payload" || isTempQueueEntry(entry.Name()) {
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			return nil, errors.New("unexpected agent queue entry")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
			return nil, errors.New("invalid agent queue record")
		}
		data, err := os.ReadFile(filepath.Join(q.root, recordsDirectory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var op operation
		if err := decode(data, &op); err != nil {
			return nil, fmt.Errorf("invalid agent queue record %s: %w", entry.Name(), err)
		}
		if err := agentproto.ValidateOperationRecord(op.OperationRecord); err != nil || deploy.ValidateRequest(op.Request) != nil || op.OperationID+".json" != entry.Name() || op.PayloadBytes < 0 || op.PayloadBytes > q.policy.MaxStagedBytes || (op.PayloadBytes == 0) != (op.PayloadSHA256 == "") || (op.PayloadSHA256 != "" && !buildmeta.ValidID(op.PayloadSHA256)) {
			return nil, fmt.Errorf("invalid agent queue record %s", entry.Name())
		}
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt.Before(ops[j].CreatedAt) })
	return ops, nil
}

func decode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func (q *Queue) saveLocked(op operation) error {
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(q.root, recordsDirectory)
	f, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		data = append(data, '\n')
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, q.recordPath(op.OperationID)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", errRecordSync, err)
	}
	close(q.changed)
	q.changed = make(chan struct{})
	return nil
}

// Submit persists a queued operation. The supplied principal must already have
// passed agentproto.Authorize; this method repeats no authorization logic so it
// can be reused by an authenticated socket service and tests.
func (q *Queue) Submit(_ context.Context, principal agentproto.Principal, request agentproto.SubmitRequest) (agentproto.SubmitResponse, error) {
	if err := agentproto.Authorize(principal, request.Request.Action, request.Request.App); err != nil {
		return agentproto.SubmitResponse{}, fmt.Errorf("%w: %w", errSubmitForbidden, err)
	}
	if err := agentproto.ValidateSubmitRequest(request); err != nil {
		return agentproto.SubmitResponse{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	ops, err := q.recordsLocked()
	if err != nil {
		return agentproto.SubmitResponse{}, err
	}
	for _, op := range ops {
		if op.IdempotencyKey == request.IdempotencyKey {
			if op.PrincipalID != principal.ID || !reflect.DeepEqual(op.Request, request.Request) || op.PayloadBytes != request.PayloadBytes || op.PayloadSHA256 != request.PayloadSHA256 {
				return agentproto.SubmitResponse{}, errors.New("idempotency key belongs to a different operation")
			}
			return agentproto.SubmitResponse{OperationID: op.OperationID, IdempotencyKey: op.IdempotencyKey, Phase: op.Phase, QueuePosition: op.QueuePosition, Deduplicated: true}, nil
		}
	}
	queued := 0
	var stagedBytes int64
	for _, op := range ops {
		if op.Phase == agentproto.PhaseQueued {
			queued++
			if op.PayloadReady {
				stagedBytes += op.PayloadBytes
			}
		}
	}
	if queued >= q.policy.MaxQueueDepth {
		return agentproto.SubmitResponse{}, errors.New("agent queue is full")
	}
	if request.PayloadBytes > q.policy.MaxStagedBytes || stagedBytes+q.inFlightBytes+request.PayloadBytes > q.policy.MaxStagedBytes {
		return agentproto.SubmitResponse{}, errors.New("agent payload exceeds staged-byte limit")
	}
	id, err := q.newID()
	if err != nil {
		return agentproto.SubmitResponse{}, err
	}
	op := operation{OperationRecord: agentproto.OperationRecord{Version: 1, OperationID: id, IdempotencyKey: request.IdempotencyKey, Action: request.Request.Action, App: request.Request.App, Service: request.Request.Service, PrincipalID: principal.ID, Phase: agentproto.PhaseQueued, QueuePosition: queued + 1, CreatedAt: q.now().UTC()}, Request: request.Request, PayloadBytes: request.PayloadBytes, PayloadSHA256: request.PayloadSHA256, PayloadReady: request.PayloadBytes == 0}
	if err := q.saveLocked(op); err != nil {
		return agentproto.SubmitResponse{}, err
	}
	return agentproto.SubmitResponse{OperationID: id, IdempotencyKey: request.IdempotencyKey, Phase: op.Phase, QueuePosition: op.QueuePosition}, nil
}

// StorePayload atomically persists the exact staged bytes promised by submit.
// A disconnected transfer is marked uncertain rather than guessed complete.
func (q *Queue) StorePayload(id string, input io.Reader) error {
	if !operationIDRE.MatchString(id) {
		return errors.New("invalid operation ID")
	}
	q.mu.Lock()
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		q.mu.Unlock()
		return err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		q.mu.Unlock()
		return err
	}
	if op.PayloadReady {
		q.mu.Unlock()
		return nil
	}
	if op.Phase != agentproto.PhaseQueued {
		q.mu.Unlock()
		return errors.New("operation is not awaiting payload")
	}
	expectedBytes := op.PayloadBytes
	expectedSHA := op.PayloadSHA256
	q.inFlightBytes += expectedBytes
	q.mu.Unlock()

	defer func() {
		q.mu.Lock()
		q.inFlightBytes -= expectedBytes
		q.mu.Unlock()
	}()

	dir := filepath.Join(q.root, recordsDirectory)
	file, err := os.CreateTemp(dir, ".payload-")
	if err != nil {
		return err
	}
	tempName := file.Name()
	defer os.Remove(tempName)

	hash := sha256.New()
	var written int64
	copyErr := file.Chmod(0600)
	if copyErr == nil {
		written, copyErr = io.CopyN(io.MultiWriter(file, hash), input, expectedBytes)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	data, err = os.ReadFile(q.recordPath(id))
	if err != nil {
		return err
	}
	if err := decode(data, &op); err != nil {
		return err
	}
	if op.Phase != agentproto.PhaseQueued {
		return errors.New("operation state changed during payload transfer")
	}
	// The byte count is the payload boundary. Any additional bytes are the
	// beginning of the following framed RPC; if they are not valid JSON the
	// daemon closes that connection. Probing one byte here would consume an
	// honest next frame and desynchronize a persistent connection.
	if copyErr != nil || written != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		now := q.now().UTC()
		op.Phase = agentproto.PhaseUncertain
		op.FinishedAt = &now
		op.Error = "agent payload transfer was incomplete or did not match its checksum"
		saveErr := q.saveLocked(op)
		if saveErr != nil {
			return saveErr
		}
		return errors.New("agent payload transfer was incomplete or did not match its checksum")
	}
	if err := os.Rename(tempName, q.payloadPath(id)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	op.PayloadReady = true
	return q.saveLocked(op)
}

// PeekQueued inspects a queued operation before concurrency slots are claimed
// so unstarted work stays in PhaseQueued while waiting for capacity.
func (q *Queue) PeekQueued(id string) (deploy.Request, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !operationIDRE.MatchString(id) {
		return deploy.Request{}, false, errors.New("invalid operation ID")
	}
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		return deploy.Request{}, false, err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		return deploy.Request{}, false, err
	}
	return op.Request, op.Phase == agentproto.PhaseQueued && op.PayloadReady, nil
}

// Start atomically claims one fully staged queued operation. The phase is
// durable before execution begins, so a daemon crash is recovered as uncertain.
func (q *Queue) Start(id string) (deploy.Request, []byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		return deploy.Request{}, nil, err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		return deploy.Request{}, nil, err
	}
	if op.Phase != agentproto.PhaseQueued || !op.PayloadReady {
		return deploy.Request{}, nil, errors.New("operation is not ready to start")
	}
	var payload []byte
	if op.PayloadBytes != 0 {
		payload, err = os.ReadFile(q.payloadPath(id))
		if err != nil || int64(len(payload)) != op.PayloadBytes {
			return deploy.Request{}, nil, errors.New("staged operation payload is unavailable")
		}
	}
	now := q.now().UTC()
	op.Phase = agentproto.PhaseRunning
	op.StartedAt = &now
	if err := q.saveLocked(op); err != nil {
		return deploy.Request{}, nil, err
	}
	return op.Request, payload, nil
}

// Finish records the exact terminal worker result and removes any staged
// private payload. Structured failure responses remain attached so clients
// preserve refusal codes, build diagnostics, and release-hook status.
func (q *Queue) Finish(id string, response deploy.Response, runErr error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		return err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		return err
	}
	if runErr != nil || response.Version == 0 {
		response = executor.FormatAppResponse(op.Request, response, runErr)
	}
	switch op.Phase {
	case agentproto.PhaseRunning:
		now := q.now().UTC()
		op.FinishedAt = &now
		op.Response = &response
		if runErr != nil || response.Error != "" {
			op.Phase = agentproto.PhaseFailed
			op.Error = response.Error
			op.ErrorCode = response.ErrorCode
		} else {
			op.Phase = agentproto.PhaseSucceeded
		}
	case agentproto.PhaseSucceeded, agentproto.PhaseFailed:
		// A sync or reconciliation error may follow a successful rename. Retry
		// the same staged worker result, never overwrite it or its timestamp.
		if !reflect.DeepEqual(op.Response, &response) {
			return errors.New("operation already has a different result")
		}
	default:
		return errors.New("operation is not running")
	}
	if err := q.saveLocked(op); err != nil {
		return err
	}
	_ = os.Remove(q.payloadPath(id))
	if op.Phase == agentproto.PhaseSucceeded && resolvesUncertain(op.Request, response) {
		if err := q.resolveUncertainLocked(op.App, op.OperationID); err != nil {
			return err
		}
	}
	return nil
}

func resolvesUncertain(req deploy.Request, response deploy.Response) bool {
	switch req.Action {
	case "deploy":
		return !req.PreviewOnly && (response.DeployPreview == nil || !response.DeployPreview.NeedsConfirmation)
	case "resume", "service-remove", "secrets-set", "backup-repository":
		return true
	case "rollback", "destroy", "cleanup":
		return req.Apply
	default:
		return false
	}
}

func (q *Queue) resolveUncertainLocked(app, resolvingID string) error {
	ops, err := q.recordsLocked()
	if err != nil {
		return err
	}
	for _, older := range ops {
		if older.OperationID == resolvingID || older.Phase != agentproto.PhaseUncertain || older.ResolvedBy != "" || older.App != app {
			continue
		}
		older.ResolvedBy = resolvingID
		if err := q.saveLocked(older); err != nil {
			return err
		}
	}
	return nil
}

// Fence records an indeterminate worker outcome and scrubs any staged payload.
// It is deliberately separate from Finish so daemon shutdown or worker death
// cannot turn an interrupted mutation into a claimed failure or success.
func (q *Queue) Fence(id, message string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		return err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		return err
	}
	if op.Phase == agentproto.PhaseUncertain && op.Error == message {
		// A previous rename may have succeeded before directory sync failed.
		return q.saveLocked(op)
	}
	if op.Phase != agentproto.PhaseRunning && op.Phase != agentproto.PhaseQueued {
		return nil
	}
	now := q.now().UTC()
	op.Phase, op.Error, op.FinishedAt = agentproto.PhaseUncertain, message, &now
	err = q.saveLocked(op)
	_ = os.Remove(q.payloadPath(id))
	return err
}

func (q *Queue) Wait(ctx context.Context, id string) (agentproto.OperationRecord, error) {
	if !operationIDRE.MatchString(id) {
		return agentproto.OperationRecord{}, errors.New("invalid operation ID")
	}
	for {
		q.mu.Lock()
		data, err := os.ReadFile(q.recordPath(id))
		if err != nil {
			q.mu.Unlock()
			return agentproto.OperationRecord{}, err
		}
		var op operation
		if err := decode(data, &op); err != nil {
			q.mu.Unlock()
			return agentproto.OperationRecord{}, err
		}
		if err := agentproto.ValidateOperationRecord(op.OperationRecord); err != nil {
			q.mu.Unlock()
			return agentproto.OperationRecord{}, err
		}
		if op.Phase != agentproto.PhaseQueued && op.Phase != agentproto.PhaseRunning {
			q.mu.Unlock()
			return op.OperationRecord, nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return agentproto.OperationRecord{}, ctx.Err()
		case <-changed:
		}
	}
}

func (q *Queue) Get(id string) (agentproto.OperationRecord, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !operationIDRE.MatchString(id) {
		return agentproto.OperationRecord{}, errors.New("invalid operation ID")
	}
	data, err := os.ReadFile(q.recordPath(id))
	if err != nil {
		return agentproto.OperationRecord{}, err
	}
	var op operation
	if err := decode(data, &op); err != nil {
		return agentproto.OperationRecord{}, err
	}
	if err := agentproto.ValidateOperationRecord(op.OperationRecord); err != nil {
		return agentproto.OperationRecord{}, err
	}
	return op.OperationRecord, nil
}

// RunningRequests returns all currently recorded running operations in creation
// order so daemon startup can inspect or re-adopt supervised worker units.
func (q *Queue) RunningRequests() (map[string]deploy.Request, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ops, err := q.recordsLocked()
	if err != nil {
		return nil, err
	}
	running := make(map[string]deploy.Request)
	for _, op := range ops {
		if op.Phase == agentproto.PhaseRunning {
			running[op.OperationID] = op.Request
		}
	}
	return running, nil
}

// Recover fences any operation that was running across agent restart without
// an active supervised worker.
func (q *Queue) Recover() error {
	_, err := q.RecoverReady(nil)
	return err
}

// RecoverReady fences orphaned running operations and incomplete uploads while
// preserving staged queued operations in FIFO order for scheduling.
func (q *Queue) RecoverReady(readopted map[string]bool) ([]string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	dir := filepath.Join(q.root, recordsDirectory)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && isTempQueueEntry(entry.Name()) {
			info, statErr := entry.Info()
			if statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	ops, err := q.recordsLocked()
	if err != nil {
		return nil, err
	}
	var ready []string
	for _, op := range ops {
		switch op.Phase {
		case agentproto.PhaseRunning:
			if readopted[op.OperationID] {
				continue
			}
			now := q.now().UTC()
			op.Phase = agentproto.PhaseUncertain
			op.FinishedAt = &now
			op.Error = "agent restarted while worker outcome was unknown"
			if err := q.saveLocked(op); err != nil {
				return nil, err
			}
			_ = os.Remove(q.payloadPath(op.OperationID))
		case agentproto.PhaseQueued:
			if op.PayloadReady {
				ready = append(ready, op.OperationID)
				continue
			}
			now := q.now().UTC()
			op.Phase = agentproto.PhaseUncertain
			op.FinishedAt = &now
			op.Error = "agent restarted before payload transfer completed"
			if err := q.saveLocked(op); err != nil {
				return nil, err
			}
			_ = os.Remove(q.payloadPath(op.OperationID))
		default:
			_ = os.Remove(q.payloadPath(op.OperationID))
		}
	}
	return ready, nil
}
