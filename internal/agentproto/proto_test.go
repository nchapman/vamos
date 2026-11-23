package agentproto

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

func validHello() HelloResponse {
	return HelloResponse{
		AgentVersion: "0.2.0",
		AgentBuildID: strings.Repeat("a", 64),
		APIMin:       1,
		APIMax:       2,
		Capabilities: []string{CapAppDeploy, CapDurableQueue, CapSecrets},
		HostID:       strings.Repeat("b", 32),
		HostName:     "prod",
		Domain:       "apps.example.com",
		Concurrency:  DefaultConcurrencyPolicy(),
	}
}

func TestHelloCompatibility(t *testing.T) {
	base := validHello()
	if err := CheckCompatibility(1, 1, []string{CapAppDeploy, CapDurableQueue}, base); err != nil {
		t.Fatalf("compatible handshake rejected: %v", err)
	}
	if err := CheckCompatibility(2, 3, []string{CapAppDeploy}, base); err != nil {
		t.Fatalf("overlapping API range rejected: %v", err)
	}

	tooOldClient := base
	tooOldClient.APIMin, tooOldClient.APIMax = 2, 3
	err := CheckCompatibility(1, 1, nil, tooOldClient)
	if err == nil || !strings.Contains(err.Error(), "upgrade local vamos CLI") {
		t.Fatalf("expected CLI upgrade guidance, got %v", err)
	}

	tooOldAgent := base
	tooOldAgent.APIMin, tooOldAgent.APIMax = 1, 1
	err = CheckCompatibility(2, 2, nil, tooOldAgent)
	if err == nil || !strings.Contains(err.Error(), "vamos server upgrade") {
		t.Fatalf("expected server upgrade guidance, got %v", err)
	}

	err = CheckCompatibility(1, 2, []string{CapPerAppConcurrency}, base)
	if err == nil || !strings.Contains(err.Error(), "vamos server upgrade") || !strings.Contains(err.Error(), CapPerAppConcurrency) {
		t.Fatalf("expected missing capability upgrade guidance, got %v", err)
	}

	dupCap := base
	dupCap.Capabilities = []string{CapAppDeploy, CapAppDeploy}
	if err := ValidateHelloResponse(dupCap); err == nil {
		t.Fatal("duplicate capability accepted")
	}
}

func TestConcurrencyPolicyValidation(t *testing.T) {
	p := DefaultConcurrencyPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("default policy rejected: %v", err)
	}
	for _, mutate := range []func(*ConcurrencyPolicy){
		func(p *ConcurrencyPolicy) { p.MaxQueueDepth = 0 },
		func(p *ConcurrencyPolicy) { p.MaxQueueDepth = 257 },
		func(p *ConcurrencyPolicy) { p.MaxStagedBytes = 1024 },
		func(p *ConcurrencyPolicy) { p.MaxConcurrentBuilds = 0 },
		func(p *ConcurrencyPolicy) { p.MaxConcurrentDeploys = 17 },
	} {
		bad := p
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid policy accepted: %+v", bad)
		}
	}
}

func TestPrincipalAuthorization(t *testing.T) {
	admin := Principal{ID: "ops-admin", Role: RoleAdmin}
	if err := Authorize(admin, "server-upgrade", ""); err != nil {
		t.Fatalf("admin refused for server-upgrade: %v", err)
	}
	if err := Authorize(admin, "deploy", "billing"); err != nil {
		t.Fatalf("admin refused for deploy: %v", err)
	}

	ci := Principal{
		ID:      "github:acme/billing:prod",
		Role:    RoleDeployer,
		Apps:    []string{"billing"},
		Actions: []string{"deploy", "status", "logs"},
	}
	if err := Authorize(ci, "deploy", "billing"); err != nil {
		t.Fatalf("CI deploy for billing refused: %v", err)
	}
	for _, denied := range []struct{ action, app string }{
		{"deploy", "studio"},
		{"secrets-set", "billing"},
		{"server-upgrade", ""},
		{"server-teardown", ""},
		{"apps", ""},
		{"unknown-action", "billing"},
	} {
		if err := Authorize(ci, denied.action, denied.app); err == nil {
			t.Fatalf("CI principal unexpectedly authorized for %+v", denied)
		}
	}

	emptyScope := Principal{ID: "ci-empty", Role: RoleDeployer}
	if err := Authorize(emptyScope, "status", "billing"); err == nil {
		t.Fatal("deployer without apps was authorized")
	}
}

func TestOperationTransitionsAndRecordValidation(t *testing.T) {
	if !CanTransition(PhaseQueued, PhaseRunning) || !CanTransition(PhaseQueued, PhaseCancelled) {
		t.Fatal("expected queued transitions")
	}
	if !CanTransition(PhaseRunning, PhaseSucceeded) || !CanTransition(PhaseRunning, PhaseFailed) || !CanTransition(PhaseRunning, PhaseUncertain) {
		t.Fatal("expected running transitions")
	}
	for _, from := range []OperationPhase{PhaseSucceeded, PhaseFailed, PhaseCancelled, PhaseUncertain} {
		for _, to := range []OperationPhase{PhaseQueued, PhaseRunning, PhaseSucceeded} {
			if CanTransition(from, to) {
				t.Fatalf("terminal/uncertain phase %s transitioned to %s", from, to)
			}
		}
	}

	rec := OperationRecord{
		Version:        1,
		OperationID:    strings.Repeat("a", 32),
		IdempotencyKey: strings.Repeat("b", 32),
		Action:         "deploy",
		App:            "billing",
		Service:        "web",
		PrincipalID:    "github:acme/billing",
		Phase:          PhaseQueued,
		QueuePosition:  1,
		CreatedAt:      time.Now().UTC(),
	}
	if err := ValidateOperationRecord(rec); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	rec.Phase = "bogus"
	if err := ValidateOperationRecord(rec); err == nil {
		t.Fatal("bogus phase accepted")
	}
}

func TestSubmitAndStrictFrameDecoding(t *testing.T) {
	req := SubmitRequest{
		IdempotencyKey: strings.Repeat("c", 32),
		Request: deploy.Request{
			Version:      1,
			Action:       "status",
			App:          "billing",
			Host:         lifecycle.Config{Name: "prod", Domain: "apps.example.com"},
			HostIdentity: "",
		},
	}
	if err := ValidateSubmitRequest(req); err != nil {
		t.Fatalf("valid submit rejected: %v", err)
	}
	_ = config.App{}
	req.IdempotencyKey = "short"
	if err := ValidateSubmitRequest(req); err == nil {
		t.Fatal("short idempotency key accepted")
	}

	var buf bytes.Buffer
	if err := EncodeFrame(&buf, RPCRequest{Version: 1, Method: MethodHello}); err != nil {
		t.Fatal(err)
	}
	var got RPCRequest
	if err := DecodeFrame(&buf, &got); err != nil || got.Method != MethodHello {
		t.Fatalf("decode failed: got=%+v err=%v", got, err)
	}

	for _, bad := range []string{
		"",
		`{"version":1,"method":"hello","unknown":true}`,
		`{"version":1,"method":"hello"}{"version":1,"method":"hello"}`,
	} {
		var out RPCRequest
		if err := DecodeFrame(strings.NewReader(bad), &out); err == nil {
			t.Fatalf("accepted invalid frame %q", bad)
		}
	}

	// Persistent streams deliver multiple newline-delimited frames without EOF
	// between them; DecodeFrame must stop at '\n' so the next frame remains intact.
	var stream bytes.Buffer
	if err := EncodeFrame(&stream, RPCRequest{Version: 1, Method: MethodHello}); err != nil {
		t.Fatal(err)
	}
	if err := EncodeFrame(&stream, RPCRequest{Version: 1, Method: MethodStatus}); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(stream.Bytes())
	var first, second RPCRequest
	if err := DecodeFrame(r, &first); err != nil || first.Method != MethodHello {
		t.Fatalf("first frame failed: got=%+v err=%v", first, err)
	}
	if err := DecodeFrame(r, &second); err != nil || second.Method != MethodStatus {
		t.Fatalf("second frame failed: got=%+v err=%v", second, err)
	}

	// Exact MaxEnvelopeBytes boundary with trailing newline succeeds; exceeding it fails.
	prefix := `{"version":1,"method":"hello","payload":"`
	suffix := "\"}\n"
	exact := prefix + strings.Repeat("x", MaxEnvelopeBytes-len(prefix)-len(suffix)) + suffix
	var sized RPCRequest
	if err := DecodeFrame(strings.NewReader(exact), &sized); err != nil {
		t.Fatalf("exact MaxEnvelopeBytes frame rejected: %v", err)
	}
	tooBig := prefix + strings.Repeat("x", MaxEnvelopeBytes-len(prefix)-len(suffix)+1) + suffix
	if err := DecodeFrame(strings.NewReader(tooBig), &sized); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestJobInboxReadPermissionAndShape(t *testing.T) {
	r := deploy.Request{Version: 1, Action: "jobs", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	p := Principal{ID: "ci", Role: RoleDeployer, Apps: []string{"billing"}}
	for _, id := range []string{"", "j_1234567890abcdef"} {
		r.JobID = id
		for _, svc := range []string{"", "web"} {
			r.Service = svc
			if r.Mutating() || ValidateReadRequest(ReadRequest{Request: r}) != nil {
				t.Fatal("invalid read", r)
			}
			for _, action := range []string{"status", "deploy", "logs"} {
				p.Actions = []string{action}
				if (Authorize(p, r.Action, r.App) == nil) != (action == "logs") || Authorize(p, r.Action, "other") == nil {
					t.Fatal("permission mismatch", action, r)
				}
			}
		}
	}
	for _, modify := range []func(*deploy.Request){
		func(q *deploy.Request) { q.JobID = "../run" },
		func(q *deploy.Request) { q.Action = "status" },
		func(q *deploy.Request) { q.Operation = strings.Repeat("1", 32) },
		func(q *deploy.Request) { q.Tail = 10 },
		func(q *deploy.Request) { q.Service = "../web" },
		func(q *deploy.Request) { q.IncludeJobs = true },
	} {
		q := r
		modify(&q)
		if ValidateReadRequest(ReadRequest{Request: q}) == nil {
			t.Fatal("accepted", q)
		}
	}
}
