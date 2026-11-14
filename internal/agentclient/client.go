// Package agentclient implements the framed client half of the installed
// coordinator protocol. SSH and future HTTPS transports only provide a
// bidirectional stream; compatibility, payload framing, and reattachment stay
// transport-independent here.
package agentclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nchapman/soar/internal/agentproto"
	"github.com/nchapman/soar/internal/buildmeta"
	"github.com/nchapman/soar/internal/deploy"
)

type Conn interface {
	io.Reader
	io.Writer
}

type Client struct {
	conn  Conn
	hello agentproto.HelloResponse
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
	request := agentproto.HelloRequest{ClientVersion: info.Label, ClientBuildID: info.ID, APIMin: agentproto.MinSupportedAPI, APIMax: agentproto.MaxSupportedAPI}
	if err := writeRequest(conn, agentproto.MethodHello, request); err != nil {
		return nil, err
	}
	var hello agentproto.HelloResponse
	if err := readResponse(conn, &hello); err != nil {
		return nil, err
	}
	if err := agentproto.CheckCompatibility(request.APIMin, request.APIMax, required, hello); err != nil {
		return nil, err
	}
	return &Client{conn: conn, hello: hello}, nil
}

func (c *Client) Hello() agentproto.HelloResponse { return c.hello }

// Submit declares and then writes an exact payload. The server persists its
// admission before consuming payload bytes; reconnecting with the same key
// therefore returns the existing operation instead of a duplicate.
func (c *Client) Submit(ctx context.Context, request deploy.Request, idempotencyKey string, payload []byte) (agentproto.SubmitResponse, error) {
	if err := ctx.Err(); err != nil {
		return agentproto.SubmitResponse{}, err
	}
	submit := agentproto.SubmitRequest{IdempotencyKey: idempotencyKey, Request: request}
	if len(payload) != 0 {
		hash := sha256.Sum256(payload)
		submit.PayloadBytes = int64(len(payload))
		submit.PayloadSHA256 = hex.EncodeToString(hash[:])
	}
	if err := writeRequest(c.conn, agentproto.MethodSubmit, submit); err != nil {
		return agentproto.SubmitResponse{}, err
	}
	var response agentproto.SubmitResponse
	if err := readResponse(c.conn, &response); err != nil {
		return agentproto.SubmitResponse{}, err
	}
	if !response.Deduplicated && len(payload) != 0 {
		if err := writeAll(ctx, c.conn, payload); err != nil {
			return agentproto.SubmitResponse{}, fmt.Errorf("send admitted payload: %w", err)
		}
	}
	return response, nil
}

func (c *Client) Status(ctx context.Context, operationID string) (agentproto.OperationRecord, error) {
	return c.operation(ctx, agentproto.MethodStatus, operationID)
}

// Wait blocks server-side until the admitted operation reaches a terminal
// phase, preserving a reconnectable operation ID rather than client polling.
func (c *Client) Wait(ctx context.Context, operationID string) (agentproto.OperationRecord, error) {
	return c.operation(ctx, agentproto.MethodWait, operationID)
}

func (c *Client) operation(ctx context.Context, method agentproto.Method, operationID string) (agentproto.OperationRecord, error) {
	if err := ctx.Err(); err != nil {
		return agentproto.OperationRecord{}, err
	}
	if err := writeRequest(c.conn, method, struct {
		OperationID string `json:"operation_id"`
	}{operationID}); err != nil {
		return agentproto.OperationRecord{}, err
	}
	var record agentproto.OperationRecord
	if err := readResponse(c.conn, &record); err != nil {
		return agentproto.OperationRecord{}, err
	}
	if err := agentproto.ValidateOperationRecord(record); err != nil {
		return agentproto.OperationRecord{}, err
	}
	return record, nil
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
func readResponse(conn Conn, dst any) error {
	var frame agentproto.RPCResponse
	if err := agentproto.DecodeFrame(conn, &frame); err != nil {
		return err
	}
	if frame.Version != agentproto.APIVersion {
		return errors.New("unsupported agent response version")
	}
	if !frame.OK {
		if frame.Error == nil {
			return errors.New("agent refused request without an error")
		}
		return fmt.Errorf("agent %s: %s", frame.Error.Code, frame.Error.Message)
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
