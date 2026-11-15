package lifecycle

import (
	"context"
	"os"
	"testing"

	"github.com/nchapman/vamos/internal/volumes"
)

func TestRetainedVolumesFenceIdentityAndPurge(t *testing.T) {
	for _, phase := range []string{"fresh", "ready", "removed"} {
		t.Run(phase, func(t *testing.T) {
			m, f := newTestManager(t)
			if phase != "fresh" {
				if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "removed" {
				if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
					t.Fatal(err)
				}
			}
			registry := m.path(volumes.Dir)
			if err := os.MkdirAll(registry, 0700); err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			_, err := m.Bootstrap(context.Background(), testConfig)
			if phase == "ready" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || len(f.calls) != 0 {
					t.Fatal("identity fence failed")
				}
			}
			f.calls = nil
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
				t.Fatal("purge accepted retained registry")
			}
			if len(f.calls) != 0 {
				t.Fatal("commands ran before fence")
			}
			if _, err := os.Stat(registry); err != nil {
				t.Fatal("registry removed")
			}
		})
	}
}
