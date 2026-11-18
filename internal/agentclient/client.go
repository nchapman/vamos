// Package agentclient implements the framed client half of the installed
// coordinator protocol. SSH and future HTTPS transports only provide a
// bidirectional stream; compatibility, payload framing, and reattachment stay
// transport-independent here.
package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/session"
	"github.com/nchapman/vamos/internal/transport"
)

type Conn interface {
	io.Reader
	io.Writer
}

type Client struct {
	conn   Conn
	reader *bufio.Reader
	hello  agentproto.HelloResponse
}

func withConnContext(ctx context.Context, conn Conn, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if closer, ok := conn.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	err := fn()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Handshake completes hello before any payload can be submitted. Unversioned
// local binaries intentionally fail here rather than mixing an ambient go run
// build with a managed server agent.
func Handshake(ctx context.Context, conn Conn, required []string) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := buildmeta.Current()
	if err != nil {
		return nil, fmt.Errorf("agent connection requires a versioned local bundle: %w", err)
	}
	reader := bufio.NewReaderSize(conn, 32*1024)
	request := agentproto.HelloRequest{ClientVersion: info.Label, ClientBuildID: info.ID, APIMin: agentproto.MinSupportedAPI, APIMax: agentproto.MaxSupportedAPI}
	var hello agentproto.HelloResponse
	err = withConnContext(ctx, conn, func() error {
		if err := writeRequest(conn, agentproto.MethodHello, request); err != nil {
			return err
		}
		return readResponse(reader, &hello)
	})
	if err != nil {
		return nil, err
	}
	if err := agentproto.CheckCompatibility(request.APIMin, request.APIMax, required, hello); err != nil {
		return nil, err
	}
	return &Client{conn: conn, reader: reader, hello: hello}, nil
}

func (c *Client) Hello() agentproto.HelloResponse { return c.hello }

// Submit declares and then writes an exact payload. The server persists its
// admission before consuming payload bytes; reconnecting with the same key
// therefore returns the existing operation instead of a duplicate.
func (c *Client) Submit(ctx context.Context, request deploy.Request, idempotencyKey string, payload []byte) (agentproto.SubmitResponse, error) {
	if err := ctx.Err(); err != nil {
		return agentproto.SubmitResponse{}, transport.NotSubmitted(err)
	}
	submit := agentproto.SubmitRequest{IdempotencyKey: idempotencyKey, Request: request}
	if len(payload) != 0 {
		hash := sha256.Sum256(payload)
		submit.PayloadBytes = int64(len(payload))
		submit.PayloadSHA256 = hex.EncodeToString(hash[:])
	}
	var response agentproto.SubmitResponse
	err := withConnContext(ctx, c.conn, func() error {
		if err := writeRequest(c.conn, agentproto.MethodSubmit, submit); err != nil {
			return err
		}
		if err := readResponse(c.reader, &response); err != nil {
			var remote *RemoteError
			if errors.As(err, &remote) {
				switch remote.Code {
				case "forbidden", "not_submitted", "invalid_request", "handshake_required", "unsupported_version", "host_unavailable":
					return transport.NotSubmitted(err)
				case "submission_refused":
					// In v0.5.0 this code also covered errors after queue-record
					// rename. Only the exact scope refusal proves non-admission.
					var principal, app string
					const scopeRefusal = `principal %q is not authorized for app %q`
					if n, _ := fmt.Sscanf(remote.Message, scopeRefusal, &principal, &app); n == 2 && app == request.App && remote.Message == fmt.Sprintf(scopeRefusal, principal, app) {
						return transport.NotSubmitted(&RemoteError{Code: "forbidden", Message: remote.Message})
					}
				}
			}
			return err
		}
		if !response.Deduplicated && len(payload) != 0 {
			if err := writeAll(ctx, c.conn, payload); err != nil {
				return fmt.Errorf("send admitted payload: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return agentproto.SubmitResponse{}, err
	}
	return response, nil
}

func (c *Client) Status(ctx context.Context, operationID string) (agentproto.OperationRecord, error) {
	return c.operation(ctx, agentproto.MethodStatus, operationID)
}

// Read executes one authorized non-mutating request directly. Reads never
// persist queue records, so polling status cannot fence teardown.
func (c *Client) Read(ctx context.Context, request deploy.Request) (deploy.Response, error) {
	if err := ctx.Err(); err != nil {
		return deploy.Response{}, err
	}
	if err := agentproto.ValidateReadRequest(agentproto.ReadRequest{Request: request}); err != nil {
		return deploy.Response{}, err
	}
	var response deploy.Response
	err := withConnContext(ctx, c.conn, func() error {
		if err := writeRequest(c.conn, agentproto.MethodRead, agentproto.ReadRequest{Request: request}); err != nil {
			return err
		}
		return readResponse(c.reader, &response)
	})
	if err != nil {
		return deploy.Response{}, err
	}
	return response, nil
}

// Wait blocks server-side until the admitted operation reaches a terminal
// phase, preserving a reconnectable operation ID rather than client polling.
func (c *Client) Wait(ctx context.Context, operationID string) (agentproto.OperationRecord, error) {
	return c.operation(ctx, agentproto.MethodWait, operationID)
}

// Session starts one interactive session. On success the returned relay owns
// the connection: JSON framing is finished and only typed stream frames
// follow until the terminal exit control frame.
func (c *Client) Session(ctx context.Context, envelope agentproto.SessionEnvelope) (session.Preparation, *Stream, error) {
	if err := ctx.Err(); err != nil {
		return session.Preparation{}, nil, err
	}
	if err := agentproto.ValidateSessionEnvelope(envelope); err != nil {
		return session.Preparation{}, nil, err
	}
	var prepared session.Preparation
	err := withConnContext(ctx, c.conn, func() error {
		if err := writeRequest(c.conn, agentproto.MethodSession, envelope); err != nil {
			return err
		}
		return readResponse(c.reader, &prepared)
	})
	if err != nil {
		return session.Preparation{}, nil, err
	}
	return prepared, &Stream{conn: c.conn, reader: c.reader, writer: c.conn}, nil
}

// Stream is the typed relay half of an attached session.
type Stream struct {
	conn   Conn
	reader *bufio.Reader
	writer io.Writer
}

// Relay pumps local stdin into typed input frames and decodes worker output
// until the exit control frame arrives. It returns the worker exit code.
//
// The input copier is intentionally unjoined: a blocked interactive terminal
// read is not interruptible, and joining it would deadlock the exit path.
// inputGuard stops the copier from touching the connection after Relay
// returns, so a later connection close ends it without a write race; a still-
// blocked read ends with the CLI process itself.
func (s *Stream) Relay(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return s.RelayWithResize(ctx, stdin, stdout, stderr, nil)
}

func (s *Stream) RelayWithResize(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, resizes <-chan agentproto.WindowSize) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	var guard inputGuard
	defer guard.stop()
	stopCancel := context.AfterFunc(ctx, func() {
		if closer, ok := s.conn.(io.Closer); ok {
			_ = closer.Close()
		}
		guard.stop()
	})
	defer stopCancel()
	done := make(chan struct{})
	defer close(done)
	if resizes != nil {
		go func() {
			for {
				select {
				case <-done:
					return
				case size, ok := <-resizes:
					if !ok {
						return
					}
					if guard.writeResize(s.writer, size) != nil {
						return
					}
				}
			}
		}()
	}
	go func() {
		if stdin == nil {
			// No input is ever sent (followed logs): signal EOF immediately so
			// the worker's stdin closes instead of blocking on a nil reader.
			_ = guard.write(s.writer, agentproto.StreamEOF, nil)
			return
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if writeErr := guard.write(s.writer, agentproto.StreamInput, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = guard.write(s.writer, agentproto.StreamEOF, nil)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	for {
		kind, data, err := agentproto.DecodeStreamChunk(s.reader)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return -1, ctxErr
			}
			return -1, err
		}
		switch kind {
		case agentproto.StreamStdout, agentproto.StreamStderr:
			out := stdout
			if kind == agentproto.StreamStderr {
				out = stderr
			}
			if _, err := out.Write(data); err != nil {
				return -1, err
			}
		case agentproto.StreamControl:
			exit, err := agentproto.DecodeSessionExit(data)
			if err != nil {
				return -1, errors.New("invalid session exit frame")
			}
			if exit.Error != "" {
				return -1, errors.New(exit.Error)
			}
			if exit.ExitCode < 0 || exit.ExitCode > 255 {
				return -1, errors.New("invalid session exit code")
			}
			return exit.ExitCode, nil
		default:
			return -1, errors.New("invalid session output frame")
		}
	}
}

func (c *Client) operation(ctx context.Context, method agentproto.Method, operationID string) (agentproto.OperationRecord, error) {
	if err := ctx.Err(); err != nil {
		return agentproto.OperationRecord{}, err
	}
	var record agentproto.OperationRecord
	err := withConnContext(ctx, c.conn, func() error {
		if err := writeRequest(c.conn, method, struct {
			OperationID string `json:"operation_id"`
		}{operationID}); err != nil {
			return err
		}
		return readResponse(c.reader, &record)
	})
	if err != nil {
		return agentproto.OperationRecord{}, err
	}
	if err := agentproto.ValidateOperationRecord(record); err != nil {
		return agentproto.OperationRecord{}, err
	}
	return record, nil
}

// inputGuard serializes and gates session input writes for one relay.
type inputGuard struct {
	mu      sync.Mutex
	stopped bool
}

func (g *inputGuard) write(w io.Writer, kind byte, data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return errors.New("session input relay stopped")
	}
	return agentproto.EncodeStreamChunk(w, kind, data)
}

func (g *inputGuard) writeResize(w io.Writer, size agentproto.WindowSize) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return errors.New("session input relay stopped")
	}
	return agentproto.EncodeStreamResize(w, size)
}

func (g *inputGuard) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
}

func writeAll(ctx context.Context, conn Conn, data []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		if setter, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			if err := setter.SetWriteDeadline(deadline); err != nil {
				return err
			}
			defer setter.SetWriteDeadline(time.Time{})
		}
	}
	for len(data) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := conn.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeRequest(conn Conn, method agentproto.Method, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return agentproto.EncodeFrame(conn, agentproto.RPCRequest{Version: agentproto.APIVersion, Method: method, Payload: data})
}

// RemoteError carries a structured coordinator refusal code while keeping the
// operator-facing Error() text identical to the underlying domain refusal.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "agent " + e.Code
}

func readResponse(r io.Reader, dst any) error {
	var frame agentproto.RPCResponse
	if err := agentproto.DecodeFrame(r, &frame); err != nil {
		return err
	}
	if frame.Version != agentproto.APIVersion {
		return errors.New("unsupported agent response version")
	}
	if !frame.OK {
		if frame.Error == nil {
			return errors.New("agent refused request without an error")
		}
		return &RemoteError{Code: frame.Error.Code, Message: frame.Error.Message}
	}
	if frame.Error != nil || len(frame.Payload) == 0 {
		return errors.New("invalid successful agent response")
	}
	dec := json.NewDecoder(bytes.NewReader(frame.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing agent response payload")
	}
	return nil
}
