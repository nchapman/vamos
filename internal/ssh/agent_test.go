package ssh

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

func TestBridgeTargetDerivation(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		err      string
	}{
		{"root@192.168.1.10", "vamos@192.168.1.10", ""},
		{"vamos@host", "vamos@host", ""},
		{"host.example.com", "vamos@host.example.com", ""},
		{"", "", "target must"},
		{"a@b@c", "", "target must"},
	} {
		got, err := bridgeTarget(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("%q: err=%v", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Fatalf("%q: got %q err=%v", tt.in, got, err)
		}
	}
}

func TestAgentRunAppValidatesBeforeDialing(t *testing.T) {
	oldDial := dialAgentBridge
	dialCalls := 0
	dialAgentBridge = func(ctx context.Context, target string) (*bridgeConn, error) {
		dialCalls++
		return nil, errors.New("connect to \"vamos@home\" over SSH: stubbed bridge dial")
	}
	t.Cleanup(func() { dialAgentBridge = oldDial })

	if _, err := AgentRunApp(context.Background(), "host;id", deploy.Request{}, nil); err == nil {
		t.Fatal("invalid target accepted")
	}
	if _, err := AgentRunApp(context.Background(), "root@home", deploy.Request{Version: 2}, nil); err == nil {
		t.Fatal("invalid request accepted")
	}
	if dialCalls != 0 {
		t.Fatalf("dialed %d times before validation", dialCalls)
	}
	request := deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	_, err := AgentRunApp(context.Background(), "root@home", request, nil)
	if err == nil || !strings.Contains(err.Error(), "connect to") || dialCalls != 1 {
		t.Fatalf("unexpected dial outcome: err=%v calls=%d", err, dialCalls)
	}
}

func TestAgentStreamAppValidatesBeforeDialing(t *testing.T) {
	oldDial := dialAgentBridge
	dialCalls := 0
	dialAgentBridge = func(ctx context.Context, target string) (*bridgeConn, error) {
		dialCalls++
		return nil, errors.New("connect to \"vamos@home\" over SSH: stubbed bridge dial")
	}
	t.Cleanup(func() { dialAgentBridge = oldDial })

	request := deploy.SessionRequest{Version: 1, Action: "logs", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Tail: 10}
	invalid := request
	invalid.Tail = 1001
	if _, err := AgentStreamApp(context.Background(), "root@home", invalid, nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("invalid session request accepted")
	}
	if _, err := AgentStreamApp(context.Background(), "host;rm -rf", request, nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("invalid target accepted")
	}
	if dialCalls != 0 {
		t.Fatalf("dialed %d times before validation", dialCalls)
	}
	_, err := AgentStreamApp(context.Background(), "root@home", request, nil, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "connect to") || dialCalls != 1 {
		t.Fatalf("unexpected dial outcome: err=%v calls=%d", err, dialCalls)
	}
}

func TestBridgeSSHArgumentsAcceptedByOpenSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	args := append([]string{"-G", "-F", "/dev/null"}, bridgeSSHArguments("vamos@192.0.2.10")...)
	cmd := exec.Command("ssh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh -G rejected bridge arguments: %v\n%s", err, out)
	}
}

func TestIdempotencyKeyForResumeDoesNotCollideWithTargetOperation(t *testing.T) {
	targetOp := strings.Repeat("a", 32)
	deployKey, err := idempotencyKeyFor(deploy.Request{Action: "deploy", Operation: targetOp})
	if err != nil || deployKey != targetOp {
		t.Fatalf("deploy key = %q err=%v", deployKey, err)
	}
	resumeKey1, err := idempotencyKeyFor(deploy.Request{Action: "resume", Operation: targetOp})
	if err != nil || len(resumeKey1) != 32 || resumeKey1 == targetOp {
		t.Fatalf("resume key1 = %q err=%v", resumeKey1, err)
	}
	resumeKey2, err := idempotencyKeyFor(deploy.Request{Action: "resume", Operation: targetOp})
	if err != nil || len(resumeKey2) != 32 || resumeKey2 == resumeKey1 || resumeKey2 == targetOp {
		t.Fatalf("resume key2 = %q err=%v", resumeKey2, err)
	}
}

func TestHostNetworkRequiresCapableAgent(t *testing.T) {
	for _, mode := range []string{"", "host"} {
		request := deploy.Request{Action: "deploy", Config: &config.App{Services: map[string]config.Service{
			"turn": {NetworkMode: mode}, "web": {Jobs: map[string]config.Job{"tick": {}}},
		}}}
		caps := requiredCapabilities(request)
		if slices.Contains(caps, agentproto.CapHostNetwork) != (mode == "host") || !slices.Contains(caps, agentproto.CapJobs) {
			t.Fatalf("required capabilities for network mode %q: %v", mode, caps)
		}
	}
}

func TestJobInboxCapability(t *testing.T) {
	for _, id := range []string{"", "j_0123456789abcdef"} {
		caps := requiredCapabilities(deploy.Request{Action: "jobs", JobID: id})
		if !slices.Contains(caps, agentproto.CapJobs) || slices.Contains(caps, agentproto.CapDurableQueue) {
			t.Fatalf("jobs requires scheduled reads, not queue: %v", caps)
		}
	}
}
