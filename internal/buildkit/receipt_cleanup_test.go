package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiptCleanupRetainsAllResources(t *testing.T) {
	for _, retained := range []string{"container", "network", "volume"} {
		t.Run(retained, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			if retained != "container" {
				f.container = nil
			}
			if retained != "network" {
				f.network = nil
			}
			if retained != "volume" {
				f.volume = nil
			}
			if err := m.ReceiptGone(context.Background(), testHost); err == nil {
				t.Fatal("declared retained resource absent")
			}
			if err := m.RemoveReceipt(context.Background(), testHost); err == nil {
				t.Fatal("removed receipt with retained resource")
			}
		})
	}
}

func TestReceiptCleanupRemovesOnlyOwnedGuardsAndMetadata(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	f.container, f.network, f.volume = nil, nil, nil
	foreign := []string{"-i", "foreign0", "-j", "DROP"}
	for key := range f.rules {
		f.rules[key] = append(f.rules[key], foreign)
	}
	if err := m.ReceiptGone(context.Background(), testHost); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveReceipt(context.Background(), testHost); err != nil {
		t.Fatal(err)
	}
	for _, rules := range f.rules {
		if len(rules) != 1 || strings.Join(rules[0], " ") != strings.Join(foreign, " ") {
			t.Fatal("foreign firewall rules changed", rules)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.root, "var/lib/soar/buildkit")); !os.IsNotExist(err) {
		t.Fatal("private metadata retained", err)
	}
	if err := m.RemoveReceipt(context.Background(), testHost); err != nil {
		t.Fatal("completed cleanup is not idempotent", err)
	}
}

func TestReceiptCleanupRefusesUnknownFilesAndChangedRules(t *testing.T) {
	for _, mode := range []string{"file", "rule", "host"} {
		t.Run(mode, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			f.container, f.network, f.volume = nil, nil, nil
			host := testHost
			switch mode {
			case "file":
				if err := os.WriteFile(filepath.Join(f.root, "var/lib/soar/buildkit/unknown"), []byte("retain me"), 0600); err != nil {
					t.Fatal(err)
				}
			case "rule":
				for key := range f.rules {
					f.rules[key] = append(f.rules[key], []string{"-m", "comment", "--comment", "soar-buildkit:" + testHost, "-j", "ACCEPT"})
				}
			case "host":
				host = strings.Repeat("f", 32)
			}
			if err := m.RemoveReceipt(context.Background(), host); err == nil {
				t.Fatal("accepted unknown or changed ownership")
			}
			if _, err := os.Stat(filepath.Join(f.root, "var/lib/soar/buildkit/state.json")); err != nil {
				t.Fatal("lost recovery receipt", err)
			}
		})
	}
}

func TestReceiptCleanupReconcilesFinalEmptyDirectory(t *testing.T) {
	m, f := newEngine(t)
	dir := filepath.Join(f.root, "var/lib/soar/buildkit")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.ReceiptGone(context.Background(), testHost); err == nil {
		t.Fatal("empty unowned directory passed preflight")
	}
	if err := m.RemoveReceipt(context.Background(), testHost); err != nil {
		t.Fatal("authorized final unlink could not resume", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("empty directory reconciliation touched Docker")
	}
}
