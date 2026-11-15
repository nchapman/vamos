package lifecycle

import (
	"context"
	"os"
	"testing"
)

func TestRetainedSecretsFenceNewIdentity(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "removed"}[removed], func(t *testing.T) {
			m, f := newTestManager(t)
			if removed {
				if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
					t.Fatal(err)
				}
				if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(m.path("/var/lib/vamos/secrets"), 0700); err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
				t.Fatal("created identity over retained secrets")
			}
			if len(f.calls) != 0 {
				t.Fatal("ran commands before fence")
			}
		})
	}
}
func TestRetainedSecretsAllowReadyReadButFenceTeardown(t *testing.T) {
	m, f := newTestManager(t)
	initial, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.path("/var/lib/vamos/secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	current, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil || current.ID != initial.ID {
		t.Fatal("retained secrets blocked ready host")
	}
	f.calls = nil
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
		t.Fatal("teardown accepted retained secrets")
	}
	if len(f.calls) != 0 {
		t.Fatal("teardown ran commands before fence")
	}
	if _, err := os.Stat(m.path("/var/lib/vamos/secrets")); err != nil {
		t.Fatal("retained secrets removed")
	}
}
