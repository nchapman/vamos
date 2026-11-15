package ssh

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
)

func TestBridgeTargetDerivation(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		err      string
	}{
		{"root@192.168.1.10", "soar@192.168.1.10", ""},
		{"soar@host", "soar@host", ""},
		{"host.example.com", "soar@host.example.com", ""},
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
	if _, err := AgentRunApp(context.Background(), "host;id", deploy.Request{}, nil); err == nil {
		t.Fatal("invalid target accepted")
	}
	request := deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	_, err := AgentRunApp(context.Background(), "root@home", request, nil)
	if err == nil {
		t.Fatal("expected bridge failure")
	}
	// ssh is unavailable in tests; the failure must surface as a connection or
	// handshake error rather than a panic or silent success.
	if !strings.Contains(err.Error(), "connect to") && !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("unexpected dial outcome: %v", err)
	}
	if _, err := AgentRunApp(context.Background(), "root@home", deploy.Request{Version: 2}, nil); err == nil {
		t.Fatal("invalid request accepted")
	}
}

func TestAgentStreamAppValidatesBeforeDialing(t *testing.T) {
	request := deploy.SessionRequest{Version: 1, Action: "logs", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Tail: 10}
	_, err := AgentStreamApp(context.Background(), "root@home", request, nil, &strings.Builder{}, &strings.Builder{})
	if err == nil {
		t.Fatal("expected bridge connection failure")
	}
	if !strings.Contains(err.Error(), "connect to") && !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("unexpected dial outcome: %v", err)
	}
	invalid := request
	invalid.Tail = 1001
	if _, err := AgentStreamApp(context.Background(), "root@home", invalid, nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("invalid session request accepted")
	}
	if _, err := AgentStreamApp(context.Background(), "host;rm -rf", request, nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("invalid target accepted")
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
