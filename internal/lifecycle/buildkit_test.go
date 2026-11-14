package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTeardownRefusesUnownedBuildServiceState(t *testing.T) {
	for _, kind := range []string{"empty", "unknown", "invalid-receipt", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			dir := m.path("/var/lib/soar/buildkit")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				filename := ""
				if kind == "unknown" {
					filename = "precious"
				} else if kind == "invalid-receipt" {
					filename = "state.json"
				}
				if filename != "" {
					if err := os.WriteFile(filepath.Join(dir, filename), []byte("retain me"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.calls = nil
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
				t.Fatal("accepted unowned build service state")
			}
			if len(f.calls) != 0 {
				t.Fatal("ran commands before rejecting unknown state")
			}
			if _, err := os.Lstat(dir); err != nil {
				t.Fatal("unknown state was removed")
			}
		})
	}
}

func TestAuthorizedTeardownResumesFinalBuildReceiptDirectoryRemoval(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	dir := m.path("/var/lib/soar/buildkit")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s.Phase, s.PurgeApproved = "removing", true
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("empty authorized receipt directory remains", err)
	}
}
