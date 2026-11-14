package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainedRepositoryStateBlocksNewHostIdentity(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	path := m.path("/var/lib/soar/backups/repositories")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "offsite.json"), []byte(`{"private":"retained-recovery-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := m.Bootstrap(context.Background(), testConfig)
	if err == nil || !strings.Contains(err.Error(), "retained backup repository") {
		t.Fatalf("retained repository did not prevent new host identity: %v", err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "apt-get") {
			t.Fatal("mutated packages before refusal")
		}
	}
}
