package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/session"
)

// peerAccountName is the only account allowed to connect to the socket. It
// is a constant in production; tests on Linux point it at the harness user so
// SO_PEERCRED is still enforced rather than skipped.
var peerAccountName = "vamos"

var bridgePrincipalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

// SessionManager is the subset of session.Manager the coordinator needs.
type SessionManager interface {
	Prepare(ctx context.Context, token string, req deploy.SessionRequest, executable string) (session.Preparation, error)
	AttachAgent(ctx context.Context, token string, initial *session.WindowSize, resizes <-chan session.WindowSize, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Cleanup(token string) error
}

// Daemon serves the coordinator's local Unix socket. SSH never executes an
// uploaded program: a restricted account runs Bridge, which relays its stdio to
// this socket and identifies the scoped principal selected by its forced-key command.
type Daemon struct {
	SocketPath         string
	StatePath          string
	PrincipalsPath     string
	AuthorizedKeysPath string
	Queue              *Queue
	Principals         map[string]agentproto.Principal
	SocketGID          int
	ScheduledJobs      bool
	// BridgePrincipal optionally pins a test or single-key installation. Production
	// uses command= options in authorized_keys to select a configured principal.
	BridgePrincipal string
	Version         string
	BuildID         string
	Capabilities    []string
	// Execute overrides worker and read execution for unit tests. When nil,
	// production launches supervised vamos-op-<id>.service units for mutations
	// and runs non-mutating reads through executor.ExecuteRead.
	Execute     func(context.Context, deploy.Request, []byte) (deploy.Response, error)
	Sessions    SessionManager
	workWG      sync.WaitGroup
	builds      chan struct{}
	deploys     chan struct{}
	exclusiveMu sync.Mutex
	locksMu     sync.Mutex
	appLocks    map[string]*sync.Mutex
}

func (d *Daemon) defaults() {
	if d.SocketPath == "" {
		d.SocketPath = "/run/vamos/agent.sock"
	}
	if d.StatePath == "" {
		d.StatePath = lifecycle.StatePath
	}
	info, err := buildmeta.Current()
	if d.Version == "" {
		d.Version = "dev"
		if err == nil {
			d.Version = info.Label
		}
	}
	if d.BuildID == "" {
		d.BuildID = strings.Repeat("0", 64)
		if err == nil {
			d.BuildID = info.ID
		}
	}
	if len(d.Capabilities) == 0 {
		d.Capabilities = []string{agentproto.CapAppDeploy, agentproto.CapHostNetwork, agentproto.CapProtectedPreview, agentproto.CapDurableQueue, agentproto.CapPerAppConcurrency, agentproto.CapScopedACL, agentproto.CapSecrets, agentproto.CapBackups, agentproto.CapSessions}
		if d.ScheduledJobs {
			d.Capabilities = append(d.Capabilities, agentproto.CapJobs)
		}
	}
	if d.Sessions == nil {
		d.Sessions = session.NewManager()
	}
	if d.SocketGID == 0 {
		// Zero is the Go default but root:root would exclude the restricted
		// vamos account. Production supplies the resolved vamos group explicitly.
		d.SocketGID = -1
	}
}

// Serve removes only a pre-existing socket, creates the controlled listener,
// and serves connections until ctx is cancelled. Queue recovery fences any
// worker that was running when a previous daemon exited.
func (d *Daemon) Serve(ctx context.Context) error {
	d.defaults()
	if d.Queue == nil {
		return errors.New("agent queue is required")
	}
	d.builds = make(chan struct{}, d.Queue.policy.MaxConcurrentBuilds)
	d.deploys = make(chan struct{}, d.Queue.policy.MaxConcurrentDeploys)
	d.appLocks = make(map[string]*sync.Mutex)
	readopted, err := d.reconcileSupervisedWorkers(ctx)
	if err != nil {
		return fmt.Errorf("reconcile running workers: %w", err)
	}
	readyIDs, err := d.Queue.RecoverReady(readopted)
	if err != nil {
		return fmt.Errorf("recover agent queue: %w", err)
	}
	for _, id := range readyIDs {
		d.schedule(ctx, id)
	}
	parent := filepath.Dir(d.SocketPath)
	if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("agent socket directory must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Lstat(d.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("agent socket path is not an owned socket")
		}
		if err := os.Remove(d.SocketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.SocketPath), 0755); err != nil {
		return err
	}
	// The service umask would mask the directory to root-only, but the
	// restricted bridge account must reach the socket to connect.
	if err := os.Chmod(filepath.Dir(d.SocketPath), 0755); err != nil {
		return err
	}
	listener, err := net.Listen("unix", d.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close(); _ = os.Remove(d.SocketPath) }()
	if err := os.Chmod(d.SocketPath, 0660); err != nil {
		return err
	}
	if d.SocketGID >= 0 {
		if err := os.Chown(d.SocketPath, 0, d.SocketGID); err != nil {
			return err
		}
	}
	var wg sync.WaitGroup
	if d.ScheduledJobs {
		jobCtx, stop := context.WithCancel(ctx)
		var clock sync.WaitGroup
		clock.Go(func() { runJobClock(jobCtx) })
		defer func() { stop(); clock.Wait() }()
	}
	// Connection handlers can schedule accepted work until they exit.
	defer d.workWG.Wait()
	defer wg.Wait()
	stopListen := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopListen()
	connSem := make(chan struct{}, 64)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case connSem <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-connSem }()
			defer conn.Close()
			stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stopConn()
			_ = d.serveConn(ctx, conn)
		}()
	}
}

func (d *Daemon) resolvePrincipals() (map[string]agentproto.Principal, error) {
	if d.PrincipalsPath == "" {
		return d.Principals, nil
	}
	if d.AuthorizedKeysPath != "" {
		return bridgeacl.KeyedPrincipals(d.PrincipalsPath, d.AuthorizedKeysPath)
	}
	return ReadPrincipals(d.PrincipalsPath)
}

func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) error {
	uid, err := peerUID(conn)
	if err != nil {
		return fmt.Errorf("read agent socket peer credentials: %w", err)
	}
	if uid >= 0 {
		account, err := user.Lookup(peerAccountName)
		if err != nil {
			return fmt.Errorf("look up restricted agent account: %w", err)
		}
		expectedUID, err := strconv.Atoi(account.Uid)
		if err != nil || uid != expectedUID {
			return errors.New("agent socket peer is not the restricted vamos account")
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	reader := bufio.NewReaderSize(conn, agentproto.MaxEnvelopeBytes+256)
	line, err := reader.ReadString('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return err
	}
	const prefix = "VAMOS-BRIDGE "
	principalID := ""
	if len(line) > len(prefix)+1 && len(line) <= len(prefix)+129 && line[:len(prefix)] == prefix {
		principalID = line[len(prefix) : len(line)-1]
	}
	if !bridgePrincipalRE.MatchString(principalID) || (d.BridgePrincipal != "" && principalID != d.BridgePrincipal) {
		return errors.New("invalid bridge principal")
	}
	helloDone := false
	for {
		var frame agentproto.RPCRequest
		if err := agentproto.DecodeFrame(reader, &frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if frame.Version != agentproto.APIVersion {
			_ = writeError(conn, "unsupported_version", "agent API version 1 is required")
			return nil
		}
		if !helloDone && frame.Method != agentproto.MethodHello {
			_ = writeError(conn, "handshake_required", "hello is required before other agent methods")
			return nil
		}
		// A persistent bridge must not keep admitting work with a revoked ACL.
		// Requests already admitted (including interactive sessions) may finish.
		principals, err := d.resolvePrincipals()
		if err != nil {
			return writeError(conn, "host_unavailable", "agent access configuration is unavailable")
		}
		principal, ok := principals[principalID]
		if !ok || principal.ID != principalID || principal.Validate() != nil {
			return writeError(conn, "forbidden", "unknown bridge principal")
		}
		switch frame.Method {
		case agentproto.MethodHello:
			if helloDone {
				_ = writeError(conn, "invalid_request", "hello was already completed")
				return nil
			}
			var request agentproto.HelloRequest
			if decodePayload(frame.Payload, &request) != nil || request.APIMin < 1 || request.APIMax < request.APIMin {
				return writeError(conn, "invalid_request", "invalid hello request")
			}
			state, stateErr := lifecycle.ReadState(d.StatePath)
			if stateErr != nil || state == nil {
				return writeError(conn, "host_unavailable", "agent host ownership state is unavailable")
			}
			response := agentproto.HelloResponse{AgentVersion: d.Version, AgentBuildID: d.BuildID, APIMin: agentproto.MinSupportedAPI, APIMax: agentproto.MaxSupportedAPI, Capabilities: d.Capabilities, HostID: state.ID, HostName: state.Config.Name, Domain: state.Config.Domain, Concurrency: d.Queue.policy}
			if err := writeOK(conn, response); err != nil {
				return err
			}
			helloDone = true
		case agentproto.MethodSubmit:
			var request agentproto.SubmitRequest
			if err := decodePayload(frame.Payload, &request); err != nil || agentproto.ValidateSubmitRequest(request) != nil || !request.Request.Mutating() {
				_ = writeError(conn, "invalid_request", "invalid submit request")
				continue
			}
			response, err := d.Queue.Submit(ctx, principal, request)
			if err != nil {
				code := "not_submitted"
				if errors.Is(err, errRecordSync) {
					code = "admission_uncertain"
				} else if errors.Is(err, errSubmitForbidden) {
					code = "forbidden"
				}
				_ = writeError(conn, code, err.Error())
				continue
			}
			awaitingUpload := !response.Deduplicated && request.PayloadBytes != 0
			if !response.Deduplicated && !awaitingUpload {
				// Admission, not delivery of the acknowledgment, owns ready work.
				d.schedule(ctx, response.OperationID)
			}
			err = writeOK(conn, response)
			if awaitingUpload {
				if err == nil {
					err = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
				}
				if err == nil {
					err = d.Queue.StorePayload(response.OperationID, reader)
					_ = conn.SetReadDeadline(time.Time{})
				}
				if err != nil {
					return errors.Join(err, d.Queue.Fence(response.OperationID, "admitted payload transfer did not complete"))
				}
				d.schedule(ctx, response.OperationID)
			}
			if err != nil {
				return err
			}
		case agentproto.MethodRead:
			var request agentproto.ReadRequest
			if err := decodePayload(frame.Payload, &request); err != nil || agentproto.ValidateReadRequest(request) != nil {
				_ = writeError(conn, "invalid_request", "invalid read request")
				continue
			}
			if err := agentproto.Authorize(principal, request.Request.Action, request.Request.App); err != nil {
				_ = writeError(conn, "forbidden", err.Error())
				continue
			}
			var (
				response deploy.Response
				runErr   error
			)
			if d.Execute != nil {
				response, runErr = d.Execute(ctx, request.Request, nil)
			} else {
				response, runErr = executor.ExecuteRead(ctx, request.Request)
			}
			if runErr != nil {
				_ = writeError(conn, "read_failed", runErr.Error())
				continue
			}
			if err := writeOK(conn, response); err != nil {
				return err
			}
		case agentproto.MethodSession:
			var envelope agentproto.SessionEnvelope
			if err := decodePayload(frame.Payload, &envelope); err != nil || agentproto.ValidateSessionEnvelope(envelope) != nil {
				_ = writeError(conn, "invalid_request", "invalid session request")
				continue
			}
			action := "exec"
			if envelope.Request.Action == "logs" {
				action = "logs"
			}
			if err := agentproto.Authorize(principal, action, envelope.Request.App); err != nil {
				_ = writeError(conn, "forbidden", err.Error())
				continue
			}
			if err := d.serveSession(ctx, conn, reader, envelope); err != nil {
				return err
			}
			// A session consumes the connection: its typed relay ends only with a
			// terminal exit frame, so no further JSON frames follow.
			return nil
		case agentproto.MethodStatus, agentproto.MethodWait:
			var request struct {
				OperationID string `json:"operation_id"`
			}
			if err := decodePayload(frame.Payload, &request); err != nil {
				_ = writeError(conn, "invalid_request", "invalid operation query")
				continue
			}
			record, err := d.Queue.Get(request.OperationID)
			if err != nil || record.PrincipalID != principal.ID {
				_ = writeError(conn, "not_found", "operation is not available to this principal")
				continue
			}
			if frame.Method == agentproto.MethodWait {
				record, err = d.Queue.Wait(ctx, request.OperationID)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					_ = writeError(conn, "not_found", "operation is not available to this principal")
					continue
				}
			}
			if err := writeOK(conn, record); err != nil {
				return err
			}
		default:
			_ = writeError(conn, "unsupported_method", "unsupported agent method")
		}
	}
}

// serveSession prepares one interactive session, validates its admission
// snapshot, then relays typed stream frames between the client and the attach
// worker until the worker exits. The exit status is a typed control frame, so
// workload output cannot forge it.
func (d *Daemon) serveSession(ctx context.Context, conn net.Conn, reader *bufio.Reader, envelope agentproto.SessionEnvelope) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	prepared, prepErr := d.Sessions.Prepare(ctx, envelope.Token, envelope.Request, "")
	if prepErr == nil && (prepared.Version != 1 || prepared.Token != envelope.Token || prepared.Error != "" || !session.ValidTarget(envelope.Request, prepared.Target)) {
		_ = d.Sessions.Cleanup(envelope.Token)
		prepErr = errors.New("invalid session preparation")
	}
	if prepErr != nil {
		return writeError(conn, "session_refused", prepErr.Error())
	}
	defer func() { _ = d.Sessions.Cleanup(envelope.Token) }()
	if err := writeOK(conn, prepared); err != nil {
		return err
	}
	var initial *session.WindowSize
	if envelope.Window != nil {
		initial = &session.WindowSize{Rows: envelope.Window.Rows, Cols: envelope.Window.Cols}
	}
	var resizeCh chan session.WindowSize
	if envelope.Request.TTY {
		resizeCh = make(chan session.WindowSize, 4)
	}
	stdin, stdinWriter := io.Pipe()
	defer stdin.Close()
	var writeMu sync.Mutex
	frame := func(kind byte) io.Writer { return chunkWriter{conn: conn, kind: kind, mu: &writeMu} }
	type attachResult struct {
		code int
		err  error
	}
	attached := make(chan attachResult, 1)
	go func() {
		code, err := d.Sessions.AttachAgent(ctx, envelope.Token, initial, resizeCh, stdin, frame(agentproto.StreamStdout), frame(agentproto.StreamStderr))
		_ = stdin.Close()
		attached <- attachResult{code, err}
	}()
	go func() {
		defer stdinWriter.Close()
		if resizeCh != nil {
			defer close(resizeCh)
		}
		closed := false
		for {
			kind, data, err := agentproto.DecodeStreamChunk(reader)
			if err != nil {
				// A broken connection must not leave a detached worker attached
				// forever; after a legitimate EOF the loop only drains until the
				// socket closes.
				cancel()
				return
			}
			switch kind {
			case agentproto.StreamEOF:
				if !closed {
					closed = true
					stdinWriter.Close()
				}
			case agentproto.StreamResize:
				size, err := agentproto.DecodeStreamResize(data)
				if err != nil || resizeCh == nil {
					stdinWriter.CloseWithError(errors.New("invalid session resize frame"))
					cancel()
					return
				}
				next := session.WindowSize{Rows: size.Rows, Cols: size.Cols}
				select {
				case resizeCh <- next:
				default:
					select {
					case <-resizeCh:
					default:
					}
					select {
					case resizeCh <- next:
					default:
					}
				}
			case agentproto.StreamInput:
				if closed {
					stdinWriter.CloseWithError(errors.New("invalid session input frame"))
					cancel()
					return
				}
				if _, err := stdinWriter.Write(data); err != nil {
					return
				}
			default:
				stdinWriter.CloseWithError(errors.New("invalid session input frame"))
				cancel()
				return
			}
		}
	}()
	result := <-attached
	// Remove session staging synchronously before transmitting the terminal
	// exit frame so callers immediately observe clean /run/vamos-sessions state.
	_ = d.Sessions.Cleanup(envelope.Token)
	exit := agentproto.SessionExit{ExitCode: result.code}
	if result.err != nil || result.code < 0 || result.code > 255 {
		exit.ExitCode = -1
		exit.Error = "session execution failed"
		if result.err != nil {
			exit.Error = result.err.Error()
		}
	}
	writeMu.Lock()
	writeErr := agentproto.EncodeStreamChunk(conn, agentproto.StreamControl, mustJSON(exit))
	writeMu.Unlock()
	cancel()
	return writeErr
}

type chunkWriter struct {
	conn net.Conn
	kind byte
	mu   *sync.Mutex
}

func (w chunkWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		size := min(len(data), agentproto.MaxStreamChunk)
		w.mu.Lock()
		err := agentproto.EncodeStreamChunk(w.conn, w.kind, data[:size])
		w.mu.Unlock()
		if err != nil {
			return written, err
		}
		data = data[size:]
		written += size
	}
	return written, nil
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func (d *Daemon) schedule(ctx context.Context, id string) {
	d.workWG.Add(1)
	go func() { defer d.workWG.Done(); d.execute(ctx, id) }()
}

func (d *Daemon) execute(ctx context.Context, id string) {
	peeked, ready, err := d.Queue.PeekQueued(id)
	if err != nil || !ready {
		return
	}
	// Claim concurrency slots BEFORE transitioning from PhaseQueued to
	// PhaseRunning: if the daemon stops while an operation is waiting for
	// capacity, it stays cleanly queued for the next daemon rather than being
	// falsely fenced as uncertain.
	releaseSlots, err := d.acquireOperationSlots(ctx, peeked)
	if err != nil {
		return
	}
	defer releaseSlots()

	request, payload, err := d.Queue.Start(id)
	if err != nil {
		if strings.Contains(err.Error(), "staged operation payload is unavailable") {
			_ = d.Queue.Fence(id, err.Error())
		}
		return
	}
	if d.Execute == nil {
		d.runSupervisedWorker(ctx, id, request, payload)
		return
	}
	workCtx, stop := context.WithTimeout(ctx, 21*time.Minute)
	defer stop()
	response, runErr := d.Execute(workCtx, request, payload)
	if ctx.Err() != nil {
		_ = d.Queue.Fence(id, "agent stopped while worker outcome was unknown")
		return
	}
	_ = d.Queue.Finish(id, response, runErr)
}

func (d *Daemon) acquireOperationSlots(ctx context.Context, request deploy.Request) (func(), error) {
	if !request.Mutating() {
		return func() {}, nil
	}
	return d.tryAcquireOperationSlots(ctx, request, true)
}

func (d *Daemon) tryAcquireOperationSlots(ctx context.Context, request deploy.Request, wait bool) (func(), error) {
	if !request.Mutating() {
		return func() {}, nil
	}
	acquireSlot := func(sem chan struct{}) bool {
		if !wait {
			select {
			case sem <- struct{}{}:
				return true
			default:
				return false
			}
		}
		return acquire(ctx, sem) == nil
	}
	lockMutex := func(mu *sync.Mutex) bool {
		if !wait {
			return mu.TryLock()
		}
		mu.Lock()
		return true
	}
	exclusive := request.App == "" || request.Action == "destroy" || request.Action == "service-remove"
	if exclusive {
		if !lockMutex(&d.exclusiveMu) {
			return nil, errors.New("exclusive slot unavailable")
		}
		acquired := 0
		for acquired < cap(d.deploys) {
			if !acquireSlot(d.deploys) {
				for i := 0; i < acquired; i++ {
					release(d.deploys)
				}
				d.exclusiveMu.Unlock()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errors.New("deploy slots unavailable")
			}
			acquired++
		}
		var appMu *sync.Mutex
		if request.App != "" {
			appMu = d.appLock(request.App)
			if !lockMutex(appMu) {
				for i := 0; i < acquired; i++ {
					release(d.deploys)
				}
				d.exclusiveMu.Unlock()
				return nil, errors.New("app slot unavailable")
			}
		}
		return func() {
			if appMu != nil {
				appMu.Unlock()
			}
			for i := 0; i < acquired; i++ {
				release(d.deploys)
			}
			d.exclusiveMu.Unlock()
		}, nil
	}

	if !acquireSlot(d.deploys) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("deploy slot unavailable")
	}
	appMu := d.appLock(request.App)
	if !lockMutex(appMu) {
		release(d.deploys)
		return nil, errors.New("app slot unavailable")
	}
	hasBuild := false
	if request.Action == "deploy" || request.Action == "run" || request.Action == "resume" {
		if !acquireSlot(d.builds) {
			appMu.Unlock()
			release(d.deploys)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("build slot unavailable")
		}
		hasBuild = true
	}
	return func() {
		if hasBuild {
			release(d.builds)
		}
		appMu.Unlock()
		release(d.deploys)
	}, nil
}

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func release(sem chan struct{}) { <-sem }
func (d *Daemon) appLock(app string) *sync.Mutex {
	d.locksMu.Lock()
	defer d.locksMu.Unlock()
	lock := d.appLocks[app]
	if lock == nil {
		lock = &sync.Mutex{}
		d.appLocks[app] = lock
	}
	return lock
}

func decodePayload(data json.RawMessage, dst any) error {
	if len(data) == 0 {
		return errors.New("missing payload")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing payload")
	}
	return nil
}
func writeOK(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return agentproto.EncodeFrame(w, agentproto.RPCResponse{Version: agentproto.APIVersion, OK: true, Payload: data})
}
func writeError(w io.Writer, code, message string) error {
	return agentproto.EncodeFrame(w, agentproto.RPCResponse{Version: agentproto.APIVersion, OK: false, Error: &agentproto.RPCError{Code: code, Message: message}})
}

// Bridge is the only program invoked by the restricted SSH account. Principal
// is supplied by an authorized_keys/ForceCommand entry, not client input.
func Bridge(ctx context.Context, socket, principal string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !bridgePrincipalRE.MatchString(principal) {
		return errors.New("invalid bridge principal")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "VAMOS-BRIDGE %s\n", principal); err != nil {
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	inputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		if closeWrite, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = closeWrite.CloseWrite()
		}
		inputDone <- err
	}()
	_, outErr := io.Copy(stdout, conn)
	if err := ctx.Err(); err != nil {
		return err
	}
	if outErr != nil {
		return outErr
	}
	select {
	case inErr := <-inputDone:
		return inErr
	default:
		// The remote side closed first. Do not wait forever for an arbitrary
		// stdin reader; closing conn causes the relay write to fail once read.
		return nil
	}
}
