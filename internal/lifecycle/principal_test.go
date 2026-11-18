package lifecycle

import (
	"context"
	"testing"
)

func TestPrincipalACLFence(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	sha := "wrong image"
	refused := func(cfg Config) {
		t.Helper()
		if err := m.WithPrincipalACL(ctx, cfg, sha, func(string) error { t.Fatal("ACL accessed outside ready installation"); return nil }); err == nil {
			t.Fatal("expected refusal")
		}
	}
	refused(testConfig)
	s, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	refused(testConfig)
	sha = s.ExecutorSHA
	other := testConfig
	other.Name = "other"
	refused(other)
	called := false
	if err = m.WithPrincipalACL(ctx, testConfig, sha, func(dir string) error {
		called = true
		if dir != m.path("/etc/vamos") {
			t.Fatalf("unexpected directory %s", dir)
		}
		refused(testConfig)
		return nil
	}); err != nil || !called {
		t.Fatalf("access=%v err=%v", called, err)
	}
	s.Phase, s.Step = "installing", "configure"
	if err = m.save(s); err != nil {
		t.Fatal(err)
	}
	refused(testConfig)
}
