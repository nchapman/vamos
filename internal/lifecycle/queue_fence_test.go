package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTeardownQueueFence verifies that finished coordinator operations do not
// block teardown while unfinished or uncertain ones do.
func TestTeardownQueueFence(t *testing.T) {
	for _, tt := range []struct {
		phase string
		block bool
	}{
		{"succeeded", false},
		{"failed", false},
		{"cancelled", false},
		{"queued", true},
		{"running", true},
		{"uncertain", true},
		{"garbage", true},
	} {
		m, _ := newTestManager(t)
		if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
			t.Fatal(err)
		}
		operations := m.path("/var/lib/vamos/queue/operations")
		if err := os.MkdirAll(operations, 0700); err != nil {
			t.Fatal(err)
		}
		record := `{"version":1,"operation_id":"0123456789abcdef0123456789abcdef","phase":"` + tt.phase + `"}`
		if err := os.WriteFile(filepath.Join(operations, "0123456789abcdef0123456789abcdef.json"), []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := m.Teardown(context.Background(), testConfig.Name, true)
		if tt.block {
			if err == nil || !strings.Contains(err.Error(), "queue") {
				t.Fatalf("%s did not block teardown: %v", tt.phase, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s blocked teardown: %v", tt.phase, err)
		}
		if _, err := os.Lstat(m.path("/var/lib/vamos/queue")); !os.IsNotExist(err) {
			t.Fatalf("teardown left /var/lib/vamos/queue behind: %v", err)
		}
	}
}

func TestTeardownAllowsResolvedUncertainOperationOnlyWhenResolverSucceeded(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	operations := m.path("/var/lib/vamos/queue/operations")
	if err := os.MkdirAll(operations, 0700); err != nil {
		t.Fatal(err)
	}
	uID := strings.Repeat("a", 32)
	rID := strings.Repeat("b", 32)
	uRecord := `{"version":1,"operation_id":"` + uID + `","phase":"uncertain","resolved_by":"` + rID + `"}`
	if err := os.WriteFile(filepath.Join(operations, uID+".json"), []byte(uRecord), 0600); err != nil {
		t.Fatal(err)
	}
	// Forged resolved_by without a succeeded resolver still blocks teardown.
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
		t.Fatal("uncertain operation with missing resolver did not block teardown")
	}
	rRecord := `{"version":1,"operation_id":"` + rID + `","phase":"succeeded"}`
	if err := os.WriteFile(filepath.Join(operations, rID+".json"), []byte(rRecord), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatalf("resolved uncertain operation blocked teardown: %v", err)
	}
	if _, err := os.Lstat(m.path("/var/lib/vamos/queue")); !os.IsNotExist(err) {
		t.Fatalf("teardown left /var/lib/vamos/queue behind: %v", err)
	}
}
