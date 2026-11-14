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

	"github.com/nchapman/soar/internal/agentproto"
	"github.com/nchapman/soar/internal/buildmeta"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
)

var bridgePrincipalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

// Daemon serves the coordinator's local Unix socket. SSH never executes an
// uploaded program: a restricted account runs Bridge, which relays its stdio to
// this socket and identifies the fixed principal named by its ForceCommand.
type Daemon struct {
	SocketPath string
	StatePath  string
	Queue      *Queue
	Principals map[string]agentproto.Principal
	SocketGID  int
	// BridgePrincipal is the only principal the installed fixed ForceCommand
	// may assert. Scoped per-key principal mapping is added with ACL management.
	BridgePrincipal string
	Version         string
	BuildID         string
	Capabilities    []string
	// Execute performs one admitted worker. Production defaults to the shared
	// engine; the serial coordinator fence remains outside this callback.
	Execute func(context.Context, deploy.Request, []byte) (deploy.Response, error)
	workMu  sync.Mutex
	workWG  sync.WaitGroup
}

func (d *Daemon) defaults() {
	if d.SocketPath == "" {
		d.SocketPath = "/run/soar/agent.sock"
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
		d.Capabilities = []string{agentproto.CapAppDeploy, agentproto.CapDurableQueue, agentproto.CapSessions}
	}
	if d.BridgePrincipal == "" {
		d.BridgePrincipal = "default-deployer"
	}
	if d.Execute == nil {
		d.Execute = deploy.NewManager().Execute
	}
	if d.SocketGID == 0 {
		// Zero is the Go default but root:root would exclude the restricted
		// soar account. Production supplies the resolved soar group explicitly.
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
	if err := d.Queue.Recover(); err != nil {
		return fmt.Errorf("recover agent queue: %w", err)
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
	listener, err := net.Listen("unix", d.SocketPath)
	if err != nil {
		return err
	}
	defer func() { listener.Close(); os.Remove(d.SocketPath) }()
	if err := os.Chmod(d.SocketPath, 0660); err != nil {
		return err
	}
	if d.SocketGID >= 0 {
		if err := os.Chown(d.SocketPath, 0, d.SocketGID); err != nil {
			return err
		}
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer d.workWG.Wait()
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer conn.Close(); _ = d.serveConn(ctx, conn) }()
	}
}

func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) error {
	uid, err := peerUID(conn)
	if err != nil {
		return fmt.Errorf("read agent socket peer credentials: %w", err)
	}
	if uid >= 0 {
		account, err := user.Lookup("soar")
		if err != nil {
			return fmt.Errorf("look up restricted agent account: %w", err)
		}
		expectedUID, err := strconv.Atoi(account.Uid)
		if err != nil || uid != expectedUID {
			return errors.New("agent socket peer is not the restricted soar account")
		}
	}
	reader := bufio.NewReaderSize(conn, agentproto.MaxEnvelopeBytes+256)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	const prefix = "SOAR-BRIDGE "
	principalID := ""
	if len(line) > len(prefix)+1 && line[:len(prefix)] == prefix {
		principalID = line[len(prefix) : len(line)-1]
	}
	if !bridgePrincipalRE.MatchString(principalID) || principalID != d.BridgePrincipal {
		return errors.New("invalid bridge principal")
	}
	principal, ok := d.Principals[principalID]
	if !ok || principal.ID != principalID || principal.Validate() != nil {
		return errors.New("unknown bridge principal")
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
		switch frame.Method {
		case agentproto.MethodHello:
			if helloDone {
				_ = writeError(conn, "invalid_request", "hello was already completed")
				return nil
			}
			var request agentproto.HelloRequest
			if err := decodePayload(frame.Payload, &request); err != nil || request.APIMin < 1 || request.APIMax < request.APIMin {
				_ = writeError(conn, "invalid_request", "invalid hello request")
				return nil
			}
			state, err := lifecycle.ReadState(d.StatePath)
			if err != nil || state == nil {
				_ = writeError(conn, "host_unavailable", "agent host ownership state is unavailable")
				return nil
			}
			response := agentproto.HelloResponse{AgentVersion: d.Version, AgentBuildID: d.BuildID, APIMin: agentproto.MinSupportedAPI, APIMax: agentproto.MaxSupportedAPI, Capabilities: d.Capabilities, HostID: state.ID, HostName: state.Config.Name, Domain: state.Config.Domain, Concurrency: d.Queue.policy}
			if err := writeOK(conn, response); err != nil {
				return err
			}
			helloDone = true
		case agentproto.MethodSubmit:
			var request agentproto.SubmitRequest
			if err := decodePayload(frame.Payload, &request); err != nil {
				_ = writeError(conn, "invalid_request", "invalid submit request")
				continue
			}
			response, err := d.Queue.Submit(ctx, principal, request)
			if err != nil {
				_ = writeError(conn, "submission_refused", err.Error())
				continue
			}
			if err := writeOK(conn, response); err != nil {
				return err
			}
			if request.PayloadBytes != 0 {
				// The fixed byte count is part of the accepted control frame, so
				// the following frame remains aligned on a persistent connection.
				if err := conn.SetReadDeadline(time.Now().Add(5 * time.Minute)); err != nil {
					return err
				}
				err := d.Queue.StorePayload(response.OperationID, reader)
				_ = conn.SetReadDeadline(time.Time{})
				if err != nil {
					return err
				}
			}
			if !response.Deduplicated {
				d.workWG.Add(1)
				go func() { defer d.workWG.Done(); d.execute(ctx, response.OperationID) }()
			}
		case agentproto.MethodStatus, agentproto.MethodWait:
			var request struct {
				OperationID string `json:"operation_id"`
			}
			if err := decodePayload(frame.Payload, &request); err != nil {
				_ = writeError(conn, "invalid_request", "invalid operation query")
				continue
			}
			var record agentproto.OperationRecord
			if frame.Method == agentproto.MethodWait {
				record, err = d.Queue.Wait(ctx, request.OperationID)
			} else {
				record, err = d.Queue.Get(request.OperationID)
			}
			if err != nil || record.PrincipalID != principal.ID {
				_ = writeError(conn, "not_found", "operation is not available to this principal")
				continue
			}
			if err := writeOK(conn, record); err != nil {
				return err
			}
		default:
			_ = writeError(conn, "unsupported_method", "unsupported agent method")
		}
	}
}

func (d *Daemon) execute(ctx context.Context, id string) {
	// Phase 2 intentionally admits one worker at a time. Phase 3 replaces this
	// with configured build/deploy semaphores and per-app/ingress lock scopes.
	d.workMu.Lock()
	defer d.workMu.Unlock()
	request, payload, err := d.Queue.Start(id)
	if err != nil {
		return
	}
	response, runErr := d.Execute(ctx, request, payload)
	if ctx.Err() != nil {
		_ = d.Queue.Fence(id, "agent stopped while worker outcome was unknown")
		return
	}
	_ = d.Queue.Finish(id, response, runErr)
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
	if _, err := fmt.Fprintf(conn, "SOAR-BRIDGE %s\n", principal); err != nil {
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
