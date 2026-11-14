package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDuplicateReceiptFieldsRefused(t *testing.T) {
	for _, key := range []string{"Host", "host"} {
		t.Run(key, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			path := filepath.Join(f.root, "var/lib/soar/buildkit/state.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = append([]byte(`{"`+key+`":"`+testHost+`",`), raw[1:]...)
			if os.WriteFile(path, raw, 0600) != nil {
				t.Fatal("write")
			}
			failBuild(t, m, f)
		})
	}
}
func TestUnsafeBuildxTreeStillStopsVerifiedDaemon(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	f.container["State"] = map[string]any{"Running": true}
	path := filepath.Join(f.root, "var/lib/soar/buildkit/buildx/foreign-link")
	if os.Symlink(f.root, path) != nil {
		t.Fatal("symlink")
	}
	failBuild(t, m, f)
	if running(f) {
		t.Fatal("invalid disposable metadata prevented cleanup of verified daemon")
	}
}
func TestInvalidHostAndSnapshotterNoNamespace(t *testing.T) {
	for _, host := range []string{"", strings.ToUpper(testHost), "../../etc", strings.Repeat("0", 31)} {
		t.Run(host, func(t *testing.T) {
			m, f := newEngine(t)
			if _, err := m.Build(context.Background(), host, "df", "iid", nil); err == nil {
				t.Fatal("accepted invalid host")
			}
			if len(f.calls) != 0 {
				t.Fatal("external operation")
			}
		})
	}
	m, f := newEngine(t)
	m.Snapshotter = "overlayfs"
	if _, err := m.Build(context.Background(), testHost, "df", "iid", nil); err == nil {
		t.Fatal("accepted unsupported snapshotter")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "var/lib/soar/buildkit")); !os.IsNotExist(err) {
		t.Fatal("invalid option poisoned namespace")
	}
}
