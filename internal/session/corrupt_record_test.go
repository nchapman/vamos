//go:build !windows

package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneCorruptRecordUsesDirectoryExpiry(t *testing.T) {
	for _, data := range []string{"{truncated", strings.Repeat("x", maxMetadata+1)} {
		t.Run(map[bool]string{true: "oversized", false: "malformed"}[len(data) > maxMetadata], func(t *testing.T) {
			f := setup(t)
			f.prepare(t, testToken)
			path := filepath.Join(f.m.Root, testToken, "prepared.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			// A young damaged record consumes capacity, but cannot block all
			// other preparations or authorize an unsafe immediate deletion.
			if _, err := f.m.Prepare(context.Background(), strings.Repeat("b", 32), f.req, f.exe); err != nil {
				t.Fatalf("corrupt record blocked unrelated preparation: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("young staging removed: %v", err)
			}
			f.now = f.now.Add(preparationTTL + time.Second)
			if _, err := f.m.Prepare(context.Background(), strings.Repeat("c", 32), f.req, f.exe); err != nil {
				t.Fatal(err)
			}
			f.gone(t, testToken)
			if f.runs != 0 {
				t.Fatal("corrupt staging executed a workload")
			}
		})
	}
}
