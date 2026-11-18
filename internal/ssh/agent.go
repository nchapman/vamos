package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nchapman/vamos/internal/agentclient"
	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/session"
	"github.com/nchapman/vamos/internal/transport"
	"golang.org/x/term"
)

// BridgeUser is the restricted host account whose forced command relays stdio
// to the installed coordinator's socket.
const BridgeUser = "vamos"

// AgentTransport implements transport.Client over the restricted SSH bridge.
type AgentTransport struct{}

var _ transport.Client = (*AgentTransport)(nil)

func NewAgentTransport() *AgentTransport { return &AgentTransport{} }

func (*AgentTransport) Inspect(ctx context.Context, target string) (*host.Report, error) {
	// Agent operations authorize at the coordinator, not through sudo. This
	// optional host report remains unprivileged on the original login target;
	// the restricted bridge itself only speaks the agent protocol.
	return Inspect(ctx, target)
}

func (*AgentTransport) RunApp(ctx context.Context, target string, request deploy.Request, payload []byte) (deploy.Response, error) {
	return AgentRunApp(ctx, target, request, payload)
}

func (*AgentTransport) StreamApp(ctx context.Context, target string, request deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return AgentStreamApp(ctx, target, request, stdin, stdout, stderr)
}

// bridgeTarget rewrites a registered target ([user@]host or SSH config alias)
// to the restricted bridge account while keeping the host/alias token intact
// so user SSH config (Port, HostName, IdentityFile, ProxyJump) still applies.
func bridgeTarget(target string) (string, error) {
	if err := ValidateTarget(target); err != nil {
		return "", err
	}
	host := target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		host = target[at+1:]
	}
	if host == "" || strings.Contains(host, "@") {
		return "", errors.New("target must be an SSH alias or [user@]host")
	}
	return BridgeUser + "@" + host, nil
}

type boundedStderr struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 4096
	if len(b.data) < limit {
		take := min(len(p), limit-len(b.data))
		b.data = append(b.data, p[:take]...)
	}
	return len(p), nil
}

func (b *boundedStderr) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

// bridgeConn is one long-lived SSH stdio pipe to the forced bridge command.
// It never requests a remote pty: the agent protocol is a byte stream and pty
// translation would corrupt framing and typed session chunks.
type bridgeConn struct {
	bridge  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  *boundedStderr
	closed  sync.Mutex
	done    bool
	waitErr error
}

var dialAgentBridge = dialBridge

func bridgeSSHArguments(bridge string) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "RemoteCommand=none",
		"-o", "EscapeChar=none",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"--", bridge,
	}
}

func dialBridge(ctx context.Context, target string) (*bridgeConn, error) {
	bridge, err := bridgeTarget(target)
	if err != nil {
		return nil, err
	}
	args := bridgeSSHArguments(bridge)
	stderrBuf := &boundedStderr{}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stderr = stderrBuf
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("connect to %q over SSH: %w; verify the restricted bridge account and 'vamos server add'", bridge, err)
	}
	return &bridgeConn{bridge: bridge, cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderrBuf}, nil
}

func (c *bridgeConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *bridgeConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close idempotently closes the pipe and reaps the ssh subprocess.
func (c *bridgeConn) Close() error {
	c.closed.Lock()
	defer c.closed.Unlock()
	if c.done {
		return c.waitErr
	}
	c.done = true
	_ = c.stdin.Close()
	c.waitErr = c.cmd.Wait()
	return c.waitErr
}

func (c *bridgeConn) diagnoseFailure(target string, opErr error) error {
	waitErr := c.Close()
	stderr := c.stderr.Bytes()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 126 {
		return fmt.Errorf("server agent bridge rejected connection to %q (exit 126): verify /etc/vamos/authorized_keys forced command or run 'vamos server upgrade'", target)
	}
	if waitErr != nil && (len(stderr) > 0 || errors.As(waitErr, &exitErr)) && strings.Contains(opErr.Error(), "empty agent frame") {
		return connectionError(c.bridge, process.Result{Stderr: stderr}, waitErr)
	}
	if len(stderr) > 0 {
		return fmt.Errorf("server agent handshake failed for %q: %w; SSH stderr: %q; verify deploy key access or run 'vamos server upgrade' with a matching CLI", target, opErr, stderr)
	}
	return fmt.Errorf("server agent handshake failed for %q: %w; run 'vamos server upgrade' with a matching CLI", target, opErr)
}

func connectAgent(ctx context.Context, target string, required []string) (*agentclient.Client, func(), error) {
	conn, err := dialAgentBridge(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := agentclient.Handshake(handshakeCtx, conn, required)
	if err != nil {
		return nil, nil, conn.diagnoseFailure(target, err)
	}
	return client, func() { _ = conn.Close() }, nil
}

// AgentHello verifies deploy protocol compatibility without submitting an app request.
func AgentHello(ctx context.Context, target string) (agentproto.HelloResponse, error) {
	client, closeConn, err := connectAgent(ctx, target, []string{agentproto.CapAppDeploy, agentproto.CapDurableQueue})
	if err != nil {
		return agentproto.HelloResponse{}, err
	}
	defer closeConn()
	return client.Hello(), nil
}

func requiredCapabilities(request deploy.Request) []string {
	caps := []string{agentproto.CapAppDeploy}
	if request.Mutating() {
		caps = append(caps, agentproto.CapDurableQueue)
	}
	switch request.Action {
	case "secrets-stage", "secrets-set", "secrets-list":
		caps = append(caps, agentproto.CapSecrets)
	case "backup-plan", "backup-repository":
		caps = append(caps, agentproto.CapBackups)
	}
	return caps
}

// AgentRunApp performs one app operation through the installed coordinator:
// no client binary is uploaded, authorization is server-side, and mutations
// persist admission before execution. payload carries the same private inputs
// (build context, secret values, repository credentials) the uploaded-executor
// transport archives.
func AgentRunApp(ctx context.Context, target string, request deploy.Request, payload []byte) (deploy.Response, error) {
	if err := ValidateTarget(target); err != nil {
		return deploy.Response{}, err
	}
	if err := deploy.ValidateRequest(request); err != nil {
		return deploy.Response{}, err
	}
	client, closeConn, err := connectAgent(ctx, target, requiredCapabilities(request))
	if err != nil {
		return deploy.Response{}, err
	}
	defer closeConn()
	if request.Mutating() {
		return runMutation(ctx, client, request, payload)
	}
	response, err := client.Read(ctx, request)
	if err != nil {
		return deploy.Response{}, err
	}
	return appOutcome(request, response, nil, nil)
}

func runMutation(ctx context.Context, client *agentclient.Client, request deploy.Request, payload []byte) (deploy.Response, error) {
	key, err := idempotencyKeyFor(request)
	if err != nil {
		return deploy.Response{}, err
	}
	submitted, err := client.Submit(ctx, request, key, payload)
	if err != nil {
		return deploy.Response{}, err
	}
	record, err := client.Wait(ctx, submitted.OperationID)
	if err != nil {
		return deploy.Response{}, fmt.Errorf("operation %s was admitted; reattach with 'vamos status': %w", submitted.OperationID, err)
	}
	switch record.Phase {
	case agentproto.PhaseSucceeded, agentproto.PhaseFailed:
		if record.Response != nil {
			return appOutcome(request, *record.Response, nil, nil)
		}
		if record.Phase == agentproto.PhaseSucceeded {
			return deploy.Response{}, errors.New("agent returned a succeeded operation without its response")
		}
		message := record.Error
		if message == "" {
			message = fmt.Sprintf("operation %s failed", submitted.OperationID)
		}
		return deploy.Response{}, fmt.Errorf("remote %s failed: %q", request.Action, message)
	case agentproto.PhaseUncertain:
		return deploy.Response{}, fmt.Errorf("operation %s outcome is uncertain: %s; inspect 'vamos status' before resuming or superseding", submitted.OperationID, record.Error)
	default:
		message := record.Error
		if message == "" {
			message = fmt.Sprintf("operation %s %s", submitted.OperationID, record.Phase)
		}
		return deploy.Response{}, errors.New(message)
	}
}

func idempotencyKeyFor(request deploy.Request) (string, error) {
	// For resume, request.Operation names the earlier interrupted journal
	// operation to resume (and operators may invoke resume more than once), not
	// this submission's unique queue key.
	if request.Operation != "" && request.Action != "resume" {
		return request.Operation, nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// AgentStreamApp attaches one interactive exec or followed log stream through
// the installed coordinator's typed session relay.
func AgentStreamApp(ctx context.Context, target string, request deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := ValidateTarget(target); err != nil {
		return -1, err
	}
	if err := deploy.ValidateSessionRequest(request); err != nil {
		return -1, err
	}
	client, closeConn, err := connectAgent(ctx, target, []string{agentproto.CapAppDeploy, agentproto.CapSessions})
	if err != nil {
		return -1, err
	}
	defer closeConn()
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return -1, err
	}
	tokenHex := hex.EncodeToString(token[:])
	var initialWin *agentproto.WindowSize
	if request.TTY {
		initialWin = initialWindowSize(stdin, stdout)
	}
	prepared, stream, err := client.Session(ctx, agentproto.SessionEnvelope{Token: tokenHex, Request: request, Window: initialWin})
	if err != nil {
		return -1, err
	}
	if prepared.Error != "" {
		return -1, fmt.Errorf("prepare session: %q", prepared.Error)
	}
	if prepared.Version != 1 || prepared.Token != tokenHex || !session.ValidTarget(request, prepared.Target) {
		return -1, errors.New("invalid session preparation response")
	}
	fmt.Fprintf(stderr, "Session: app=%s service=%s container=%s candidate=%t\n", prepared.Target.App, prepared.Target.Service, prepared.Target.ContainerID, prepared.Target.Candidate)
	if !request.Stdin {
		stdin = nil
	}
	restore, err := makeRaw(request, stdin, stdout)
	if err != nil {
		return -1, err
	}
	defer restore()
	var resizes <-chan agentproto.WindowSize
	if request.TTY {
		var stopWatch func()
		resizes, stopWatch = watchWindowSize(ctx, stdin, stdout, initialWin)
		defer stopWatch()
	}
	code, err := stream.RelayWithResize(ctx, stdin, stdout, stderr, resizes)
	if err != nil {
		if request.Action == "exec" {
			return -1, errors.New("execution outcome unknown (bridge disconnected, cancelled, or exited without status); the command may still be running; do not automatically retry")
		}
		return -1, errors.New("log stream interrupted; reconnect explicitly to select the current container")
	}
	return code, nil
}

// makeRaw puts a local terminal into raw mode for interactive TTY sessions;
// with no remote pty the client owns echo and line-discipline handling.
func makeRaw(request deploy.SessionRequest, stdin io.Reader, stdout io.Writer) (func(), error) {
	if !request.TTY {
		return func() {}, nil
	}
	in, inOK := stdin.(*os.File)
	out, outOK := stdout.(*os.File)
	if !inOK || !outOK {
		return func() {}, nil
	}
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return func() {}, nil
	}
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return func() {}, err
	}
	return func() { _ = term.Restore(int(in.Fd()), state) }, nil
}
