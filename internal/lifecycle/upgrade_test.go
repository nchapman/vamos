package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeReplacesPairedBinariesOnlyAfterOwnershipValidation(t *testing.T) {
	m, _ := newTestManager(t)
	state, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	oldExecutor, oldAgent := state.ExecutorSHA, state.AgentSHA
	m.Executor = []byte("new-executor")
	m.Agent = []byte("new-agent")
	upgraded, err := m.Upgrade(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.ExecutorSHA == oldExecutor || upgraded.AgentSHA == oldAgent || upgraded.ExecutorSHA == upgraded.AgentSHA {
		t.Fatalf("upgrade did not record new paired artifacts: %+v", upgraded)
	}
	if _, err := m.Upgrade(context.Background(), Config{Name: testConfig.Name, Domain: "other.example.com"}); err == nil {
		t.Fatal("upgrade accepted a different host identity")
	}
}

func TestUpgradePreservesLiveCaddyRoutesAndRefusesBusyQueue(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	liveRoutes := caddyConfig + "\nbilling.apps.example.com {\n\treverse_proxy 127.0.0.1:12001\n}\n"
	if err := os.WriteFile(m.path("/etc/caddy/Caddyfile"), []byte(liveRoutes), 0644); err != nil {
		t.Fatal(err)
	}
	customPrincipals := "[{\"id\":\"ci-billing\",\"role\":\"deployer\",\"apps\":[\"billing\"]}]\n"
	if err := os.WriteFile(m.path("/etc/vamos/principals.json"), []byte(customPrincipals), 0600); err != nil {
		t.Fatal(err)
	}
	m.Executor = []byte("executor-v2")
	m.Agent = []byte("agent-v2")
	if _, err := m.Upgrade(context.Background(), testConfig); err != nil {
		t.Fatalf("upgrade rejected live Caddy routes or custom ACLs: %v", err)
	}

	// An active or unresolved queue operation blocks upgrade without overwriting binaries.
	opsDir := m.path("/var/lib/vamos/queue/operations")
	if err := os.MkdirAll(opsDir, 0700); err != nil {
		t.Fatal(err)
	}
	runningRecord := `{"version":1,"operation_id":"0123456789abcdef0123456789abcdef","phase":"running"}`
	if err := os.WriteFile(filepath.Join(opsDir, "0123456789abcdef0123456789abcdef.json"), []byte(runningRecord), 0600); err != nil {
		t.Fatal(err)
	}
	m.Executor = []byte("executor-v3")
	m.Agent = []byte("agent-v3")
	if _, err := m.Upgrade(context.Background(), testConfig); err == nil {
		t.Fatal("upgrade succeeded while queue had a running operation")
	}
}
