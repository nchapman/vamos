package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/agentclient"
	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/session"
)

// stampAgentBuild gives the test binary a valid embedded bundle identity so
// client handshakes pass the pairing guard.
func stampAgentBuild(t *testing.T) {
	t.Helper()
	old := buildmeta.Stamp
	oldPeer := peerAccountName
	if current, err := user.Current(); err == nil {
		// On Linux the daemon enforces SO_PEERCRED against the restricted
		// account; the harness user is not "vamos", so enforce against the real
		// connecting account instead of skipping the check.
		peerAccountName = current.Username
	}
	stamp, err := buildmeta.Encode("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	buildmeta.Stamp = stamp
	t.Cleanup(func() {
		buildmeta.Stamp = old
		peerAccountName = oldPeer
	})
}

// testDaemon starts one daemon on a real Unix socket with a stub principal and
// returns a dialed, handshake-complete client. peerUID is unavailable on this
// path, matching the localhost test expectation.
func testDaemon(t *testing.T, execute func(context.Context, deploy.Request, []byte) (deploy.Response, error), sessions SessionManager) (*agentclient.Client, string, func()) {
	t.Helper()
	queueRoot := filepath.Join(t.TempDir(), "queue")
	// macOS temp directories exceed the 104-byte unix socket path limit.
	socketDir, err := os.MkdirTemp("/tmp", "vamos-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	queue, err := NewQueue(queueRoot, agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "host.json")
	state := &lifecycle.State{Version: 1, ID: strings.Repeat("a", 32), Config: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Phase: "ready", Step: "complete", Configured: true, ExecutorSHA: strings.Repeat("b", 64), AgentSHA: strings.Repeat("c", 64), CaddyContainerID: strings.Repeat("d", 64), Accounts: map[string]string{"group:docker": "x", "passwd:vamos": "x", "group:vamos": "x"}, Packages: []string{"docker.io"}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		SocketPath: filepath.Join(socketDir, "agent.sock"),
		StatePath:  statePath,
		Queue:      queue,
		Principals: map[string]agentproto.Principal{"ci:billing": {ID: "ci:billing", Role: agentproto.RoleDeployer, Apps: []string{"billing"}}},
		Execute:    execute,
		Sessions:   sessions,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	for i := 0; i < 50; i++ {
		if conn, err := net.Dial("unix", d.SocketPath); err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, err := net.Dial("unix", d.SocketPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// The daemon expects the bridge's fixed principal announcement line.
	if _, err := conn.Write([]byte("VAMOS-BRIDGE ci:billing\n")); err != nil {
		cancel()
		t.Fatal(err)
	}
	client, err := agentclient.Handshake(context.Background(), conn, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return client, queueRoot, func() {
		conn.Close()
		cancel()
		<-done
	}
}

func statusRequest(app string) deploy.Request {
	return deploy.Request{Version: 1, Action: "status", App: app, Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
}

func TestDaemonReadExecutesAuthorizedRequestsDirectly(t *testing.T) {
	stampAgentBuild(t)
	var executed []deploy.Request
	client, queueRoot, stop := testDaemon(t, func(ctx context.Context, request deploy.Request, payload []byte) (deploy.Response, error) {
		executed = append(executed, request)
		return deploy.Response{Version: 1, Status: &deploy.Status{App: request.App}}, nil
	}, nil)
	defer stop()
	response, err := client.Read(context.Background(), statusRequest("billing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(executed) != 1 || executed[0].App != "billing" || response.Status == nil || response.Status.App != "billing" {
		t.Fatalf("unexpected read: %+v %+v", executed, response)
	}
	// Reads never persist queue records.
	if _, err := client.Read(context.Background(), statusRequest("other")); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("out-of-scope read accepted: %v", err)
	}
	if records, err := filepath.Glob(filepath.Join(queueRoot, "operations", "*.json")); err != nil || len(records) != 0 {
		t.Fatalf("read persisted queue records: %v %v", records, err)
	}
}

func TestDaemonReadRejectsMutatingActions(t *testing.T) {
	stampAgentBuild(t)
	client, _, stop := testDaemon(t, func(context.Context, deploy.Request, []byte) (deploy.Response, error) {
		return deploy.Response{}, errors.New("must not execute")
	}, nil)
	defer stop()
	request := statusRequest("billing")
	request.Action = "deploy"
	request.Operation = strings.Repeat("1", 32)
	if _, err := client.Read(context.Background(), request); err == nil {
		t.Fatal("mutating read accepted")
	}
}

func TestDaemonSubmitRejectsReadOnlyActions(t *testing.T) {
	stampAgentBuild(t)
	client, _, stop := testDaemon(t, func(context.Context, deploy.Request, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Status: &deploy.Status{App: "billing"}}, nil
	}, nil)
	defer stop()
	submitted, err := client.Submit(context.Background(), statusRequest("billing"), strings.Repeat("2", 32), nil)
	if err == nil {
		t.Fatalf("read-only submit accepted: %+v", submitted)
	}
}

type fakeSessions struct {
	mu         sync.Mutex
	prepareErr error
	prepared   session.Preparation
	cleaned    bool
	initial    *session.WindowSize
	attach     func(initial *session.WindowSize, resizes <-chan session.WindowSize, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

func (f *fakeSessions) Prepare(ctx context.Context, token string, req deploy.SessionRequest, executable string) (session.Preparation, error) {
	if f.prepareErr != nil {
		return session.Preparation{}, f.prepareErr
	}
	return f.prepared, nil
}

func (f *fakeSessions) AttachAgent(ctx context.Context, token string, initial *session.WindowSize, resizes <-chan session.WindowSize, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.initial = initial
	f.mu.Unlock()
	return f.attach(initial, resizes, stdin, stdout, stderr)
}

func (f *fakeSessions) Cleanup(token string) error {
	f.mu.Lock()
	f.cleaned = true
	f.mu.Unlock()
	return nil
}

func validPreparation(token string) session.Preparation {
	return session.Preparation{Version: 1, Token: token, Target: deploy.SessionTarget{App: "billing", Service: "web", HostIdentity: strings.Repeat("b", 32), Incarnation: strings.Repeat("c", 32), Operation: strings.Repeat("d", 32), ContainerID: strings.Repeat("e", 64)}}
}

func TestDaemonSessionRelaysTypedStreamAndExitCode(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	token := strings.Repeat("3", 32)
	sessions.prepared = validPreparation(token)
	sessions.attach = func(_ *session.WindowSize, _ <-chan session.WindowSize, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if _, err := stdout.Write([]byte("workload-output")); err != nil {
			return -1, err
		}
		if _, err := stderr.Write([]byte("diagnostic")); err != nil {
			return -1, err
		}
		input, err := io.ReadAll(stdin)
		if err != nil {
			return -1, err
		}
		if string(input) != "console-input" {
			return 9, errors.New("stdin mismatch: " + string(input))
		}
		return 7, nil
	}
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Command: []string{"true"}, Stdin: true}
	_, stream, err := client.Session(context.Background(), agentproto.SessionEnvelope{Token: token, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code, err := stream.Relay(context.Background(), strings.NewReader("console-input"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "workload-output" || stderr.String() != "diagnostic" {
		t.Fatalf("relay mismatch: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	sessions.mu.Lock()
	cleaned := sessions.cleaned
	sessions.mu.Unlock()
	if !cleaned {
		t.Fatal("session cleanup did not complete before relay returned")
	}
}

func TestDaemonSessionRefusesMismatchedPreparation(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{prepared: validPreparation(strings.Repeat("4", 32))}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	// The token names a different staging directory than the preparation
	// admits, so the daemon must refuse before attaching any command.
	token := strings.Repeat("5", 32)
	request := deploy.SessionRequest{Version: 1, Action: "logs", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Tail: 10}
	if _, _, err := client.Session(context.Background(), agentproto.SessionEnvelope{Token: token, Request: request}); err == nil {
		t.Fatal("mismatched preparation accepted")
	}
}

func TestDaemonSessionSplitsLargeWorkerWrites(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	token := strings.Repeat("7", 32)
	sessions.prepared = validPreparation(token)
	sessions.attach = func(_ *session.WindowSize, _ <-chan session.WindowSize, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		big := bytes.Repeat([]byte("x"), 3*agentproto.MaxStreamChunk+11)
		if _, err := stdout.Write(big); err != nil {
			return -1, err
		}
		return 0, nil
	}
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Command: []string{"true"}}
	_, stream, err := client.Session(context.Background(), agentproto.SessionEnvelope{Token: token, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := stream.Relay(context.Background(), nil, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || stdout.Len() != 3*agentproto.MaxStreamChunk+11 {
		t.Fatalf("large write lost data: code=%d len=%d", code, stdout.Len())
	}
}

func TestDaemonSessionEnforcesAppScope(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{prepared: validPreparation(strings.Repeat("6", 32))}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: "other", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Command: []string{"true"}}
	if _, _, err := client.Session(context.Background(), agentproto.SessionEnvelope{Token: strings.Repeat("6", 32), Request: request}); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("out-of-scope session accepted: %v", err)
	}
}

func TestDaemonSessionForwardsWindowSizeAndResizes(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	token := strings.Repeat("8", 32)
	sessions.prepared = validPreparation(token)
	resized := make(chan session.WindowSize, 1)
	sessions.attach = func(initial *session.WindowSize, resizes <-chan session.WindowSize, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if initial == nil || initial.Rows != 24 || initial.Cols != 80 {
			return 1, errors.New("unexpected initial window")
		}
		select {
		case got := <-resizes:
			resized <- got
		case <-time.After(5 * time.Second):
			return 1, errors.New("timed out waiting for resize")
		}
		_, _ = stdout.Write([]byte("resized-ok"))
		return 0, nil
	}
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Command: []string{"/bin/sh"}, Stdin: true, TTY: true}
	_, stream, err := client.Session(context.Background(), agentproto.SessionEnvelope{
		Token:   token,
		Request: request,
		Window:  &agentproto.WindowSize{Rows: 24, Cols: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	resizeCh := make(chan agentproto.WindowSize, 1)
	resizeCh <- agentproto.WindowSize{Rows: 40, Cols: 100}
	close(resizeCh)
	var stdout bytes.Buffer
	code, err := stream.RelayWithResize(context.Background(), strings.NewReader(""), &stdout, io.Discard, resizeCh)
	if err != nil {
		t.Fatal(err)
	}
	got := <-resized
	if code != 0 || got != (session.WindowSize{Rows: 40, Cols: 100}) || stdout.String() != "resized-ok" {
		t.Fatalf("pty resize mismatch: code=%d got=%+v out=%q", code, got, stdout.String())
	}
}

func TestDaemonSessionReportsAttachFailureAsError(t *testing.T) {
	stampAgentBuild(t)
	sessions := &fakeSessions{}
	client, _, stop := testDaemon(t, nil, sessions)
	defer stop()
	token := strings.Repeat("9", 32)
	sessions.prepared = validPreparation(token)
	sessions.attach = func(*session.WindowSize, <-chan session.WindowSize, io.Reader, io.Writer, io.Writer) (int, error) {
		return -1, errors.New("session target changed since preparation")
	}
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Command: []string{"true"}}
	_, stream, err := client.Session(context.Background(), agentproto.SessionEnvelope{Token: token, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Relay(context.Background(), nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "target changed") {
		t.Fatalf("expected attach error propagation, got %v", err)
	}
}

func TestDaemonPreservesQueuedWorkAcrossRestartBeforeAdmission(t *testing.T) {
	stampAgentBuild(t)
	queueRoot := filepath.Join(t.TempDir(), "queue")
	socketDir, err := os.MkdirTemp("/tmp", "vamos-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	queue, err := NewQueue(queueRoot, agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "host.json")
	state := &lifecycle.State{Version: 1, ID: strings.Repeat("a", 32), Config: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Phase: "ready", Step: "complete", Configured: true, ExecutorSHA: strings.Repeat("b", 64), AgentSHA: strings.Repeat("c", 64), CaddyContainerID: strings.Repeat("d", 64), Accounts: map[string]string{"group:docker": "x", "passwd:vamos": "x", "group:vamos": "x"}, Packages: []string{"docker.io"}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	principals := map[string]agentproto.Principal{
		"ci:all": {ID: "ci:all", Role: agentproto.RoleDeployer, Apps: []string{"*"}},
	}
	firstStarted := make(chan struct{})
	d1 := &Daemon{
		SocketPath: filepath.Join(socketDir, "agent.sock"),
		StatePath:  statePath,
		Queue:      queue,
		Principals: principals,
		Execute: func(ctx context.Context, req deploy.Request, _ []byte) (deploy.Response, error) {
			if req.App == "first" {
				close(firstStarted)
				<-ctx.Done()
				return deploy.Response{}, ctx.Err()
			}
			return deploy.Response{Version: 1, Status: &deploy.Status{App: req.App}}, nil
		},
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- d1.Serve(ctx1) }()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("unix", d1.SocketPath); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	dialClient := func() (*agentclient.Client, net.Conn) {
		conn, err := net.Dial("unix", d1.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("VAMOS-BRIDGE ci:all\n")); err != nil {
			t.Fatal(err)
		}
		cl, err := agentclient.Handshake(context.Background(), conn, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cl, conn
	}
	cl, conn := dialClient()
	op1Req := deploy.Request{Version: 1, Action: "cleanup", App: "first", Operation: strings.Repeat("1", 32), Host: state.Config}
	sub1, err := cl.Submit(context.Background(), op1Req, strings.Repeat("1", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	op2Req := deploy.Request{Version: 1, Action: "cleanup", App: "second", Operation: strings.Repeat("2", 32), Host: state.Config}
	sub2, err := cl.Submit(context.Background(), op2Req, strings.Repeat("2", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	rec2Before, err := cl.Status(context.Background(), sub2.OperationID)
	if err != nil || rec2Before.Phase != agentproto.PhaseQueued {
		t.Fatalf("second op did not remain queued while slot was full: %+v err=%v", rec2Before, err)
	}
	conn.Close()
	cancel1()
	<-done1

	queue2, err := NewQueue(queueRoot, agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	d2 := &Daemon{
		SocketPath: filepath.Join(socketDir, "agent.sock"),
		StatePath:  statePath,
		Queue:      queue2,
		Principals: principals,
		Execute: func(_ context.Context, req deploy.Request, _ []byte) (deploy.Response, error) {
			return deploy.Response{Version: 1, Status: &deploy.Status{App: req.App}}, nil
		},
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- d2.Serve(ctx2) }()
	defer func() { cancel2(); <-done2 }()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("unix", d2.SocketPath); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cl2, conn2 := dialClient()
	defer conn2.Close()

	rec1After, err := cl2.Status(context.Background(), sub1.OperationID)
	if err != nil || rec1After.Phase != agentproto.PhaseUncertain {
		t.Fatalf("interrupted running op1 was not fenced: %+v err=%v", rec1After, err)
	}
	rec2After, err := cl2.Wait(context.Background(), sub2.OperationID)
	if err != nil || rec2After.Phase != agentproto.PhaseSucceeded || rec2After.Response == nil || rec2After.Response.Status.App != "second" {
		t.Fatalf("queued op2 did not schedule and succeed after restart: %+v err=%v", rec2After, err)
	}
}

func TestDaemonDeduplicatedSubmitWithPayloadAllowsSubsequentRPCs(t *testing.T) {
	stampAgentBuild(t)
	client, _, stop := testDaemon(t, func(_ context.Context, req deploy.Request, payload []byte) (deploy.Response, error) {
		if string(payload) != "bundle-bytes" {
			return deploy.Response{}, errors.New("unexpected payload")
		}
		return deploy.Response{Version: 1, Status: &deploy.Status{App: req.App, Phase: "ready"}}, nil
	}, nil)
	defer stop()

	req := deploy.Request{
		Version:       1,
		Action:        "deploy",
		App:           "billing",
		Operation:     strings.Repeat("d", 32),
		ContextFormat: deploy.BundleFormat,
		Host:          lifecycle.Config{Name: "prod", Domain: "apps.example.com"},
		Config:        &config.App{Name: "billing", Services: map[string]config.Service{"web": {Image: "nginx:1.27@sha256:" + strings.Repeat("e", 64), HTTP: &config.HTTP{Port: 8080, Health: "/"}, Update: config.UpdateAuto}}},
	}
	idem := strings.Repeat("f", 32)
	sub1, err := client.Submit(context.Background(), req, idem, []byte("bundle-bytes"))
	if err != nil || sub1.Deduplicated {
		t.Fatalf("first submit failed: %+v err=%v", sub1, err)
	}
	sub2, err := client.Submit(context.Background(), req, idem, []byte("bundle-bytes"))
	if err != nil || !sub2.Deduplicated || sub2.OperationID != sub1.OperationID {
		t.Fatalf("deduplicated submit failed: %+v err=%v", sub2, err)
	}
	rec, err := client.Wait(context.Background(), sub1.OperationID)
	if err != nil || rec.Phase != agentproto.PhaseSucceeded {
		t.Fatalf("wait after deduplicated submit failed: %+v err=%v", rec, err)
	}
}
