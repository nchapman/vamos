package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/appstate"
)

func writeAppTombstone(t *testing.T, m *Manager, hostID string) string {
	t.Helper()
	p := m.path("/var/lib/vamos/apps/retired/state.json")
	b, err := json.Marshal(appstate.Tombstone{Version: 1, App: "retired", Operation: strings.Repeat("b", 32), Phase: "destroyed", HostID: hostID})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTeardownAcceptsOnlyCompletedAppReceipts(t *testing.T) {
	m, _ := newTestManager(t)
	state, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTombstone(t, m, state.ID)
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.path("/var/lib/vamos/apps")); !os.IsNotExist(err) {
		t.Fatalf("receipt directory remains: %v", err)
	}
}

func TestAppReceiptsCannotAuthorizeForeignDataRemoval(t *testing.T) {
	for _, kind := range []string{"foreign-host", "active-state", "extra-file", "unknown-field", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			state, err := m.Bootstrap(context.Background(), testConfig)
			if err != nil {
				t.Fatal(err)
			}
			p := writeAppTombstone(t, m, state.ID)
			switch kind {
			case "foreign-host":
				writeAppTombstone(t, m, strings.Repeat("f", 32))
			case "active-state":
				if err := os.WriteFile(p, []byte(`{"phase":"destroying"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				if err := os.WriteFile(filepath.Join(filepath.Dir(p), "keep"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b = append(b[:len(b)-1], []byte(`,"Current":null}`)...)
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "receipt")
				if err := os.Rename(p, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			}
			f.calls = nil
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
				t.Fatal("accepted unsafe app state")
			}
			if len(f.calls) != 0 {
				t.Fatalf("ran commands before rejecting unsafe receipt: %v", f.calls)
			}
		})
	}
}

func TestTeardownRefusesMountedAppReceipt(t *testing.T) {
	m, f := newTestManager(t)
	state, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTombstone(t, m, state.ID)
	m.MountInfo = func() ([]byte, error) {
		return []byte("36 25 0:32 / /var/lib/vamos/apps/retired rw - ext4 /dev/vdb rw\n"), nil
	}
	f.calls = nil
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
		t.Fatal("accepted mounted app receipt")
	}
	// Only the read-only ingress lookup that scopes the mount check may run.
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "systemctl is-active") && !strings.HasPrefix(call, "docker container ls") && !strings.HasPrefix(call, "docker container inspect") {
			t.Fatal("mutated before mount refusal", f.calls)
		}
	}
}

func TestBootstrapRefusesPendingAppStateBeforeHostCommands(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	f.write("/var/lib/vamos/apps/pending/state.json", `{"phase":"destroying"}`)
	f.calls = nil
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
		t.Fatal("bootstrap accepted pending app state")
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran host commands: %v", f.calls)
	}
}
