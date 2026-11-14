package lifecycle

import (
	"context"
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
