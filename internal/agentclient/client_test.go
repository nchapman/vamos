package agentclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/agentproto"
	"github.com/nchapman/soar/internal/buildmeta"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
)

func TestHandshakeSubmitAndReattachStatus(t *testing.T) {
	old := buildmeta.Stamp
	stamp, err := buildmeta.Encode("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	buildmeta.Stamp = stamp
	defer func() { buildmeta.Stamp = old }()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() {
		var hello agentproto.RPCRequest
		_ = agentproto.DecodeFrame(serverConn, &hello)
		helloPayload, _ := json.Marshal(agentproto.HelloResponse{AgentVersion: "test", AgentBuildID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", APIMin: 1, APIMax: 1, Capabilities: []string{agentproto.CapAppDeploy, agentproto.CapDurableQueue}, HostID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HostName: "prod", Domain: "apps.example.com", Concurrency: agentproto.DefaultConcurrencyPolicy()})
		_ = agentproto.EncodeFrame(serverConn, agentproto.RPCResponse{Version: 1, OK: true, Payload: helloPayload})
		var submit agentproto.RPCRequest
		_ = agentproto.DecodeFrame(serverConn, &submit)
		response, _ := json.Marshal(agentproto.SubmitResponse{OperationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", IdempotencyKey: "cccccccccccccccccccccccccccccccc", Phase: agentproto.PhaseQueued, QueuePosition: 1})
		_ = agentproto.EncodeFrame(serverConn, agentproto.RPCResponse{Version: 1, OK: true, Payload: response})
		var status agentproto.RPCRequest
		_ = agentproto.DecodeFrame(serverConn, &status)
		record, _ := json.Marshal(agentproto.OperationRecord{Version: 1, OperationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", IdempotencyKey: "cccccccccccccccccccccccccccccccc", Action: "status", App: "billing", PrincipalID: "ci:billing", Phase: agentproto.PhaseQueued, CreatedAt: time.Now().UTC()})
		_ = agentproto.EncodeFrame(serverConn, agentproto.RPCResponse{Version: 1, OK: true, Payload: record})
	}()
	client, err := Handshake(context.Background(), clientConn, []string{agentproto.CapAppDeploy, agentproto.CapDurableQueue})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Submit(context.Background(), deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}, "cccccccccccccccccccccccccccccccc", nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := client.Status(context.Background(), response.OperationID)
	if err != nil || record.OperationID != response.OperationID {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestStreamRelayUnblocksOnContextCancel(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	stream := &Stream{conn: clientConn, reader: bufio.NewReader(clientConn), writer: clientConn}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := stream.Relay(ctx, nil, io.Discard, io.Discard)
		done <- err
	}()
	// Drain the initial EOF frame on serverConn and then hold the stream open.
	_, _, _ = agentproto.DecodeStreamChunk(serverConn)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Relay err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream.Relay hung on context cancellation")
	}
}
