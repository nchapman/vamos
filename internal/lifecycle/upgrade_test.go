package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestUpgradeReplacesPairedBinariesOnlyAfterOwnershipValidation(t *testing.T) {
	m, _ := newTestManager(t)
	state, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	oldExecutor, oldAgent := state.ExecutorSHA, state.AgentSHA
	m.Executor = []byte("new-executor")
	m.Agent = []byte("new-agent")
	upgraded, err := m.Upgrade(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.ExecutorSHA == oldExecutor || upgraded.AgentSHA == oldAgent || upgraded.ExecutorSHA == upgraded.AgentSHA {
		t.Fatalf("upgrade did not record new paired artifacts: %+v", upgraded)
	}
	if _, err := m.Upgrade(context.Background(), Config{Name: testConfig.Name, Domain: "other.example.com"}); err == nil {
		t.Fatal("upgrade accepted a different host identity")
	}
}

func TestUpgradePreservesLiveCaddyRoutesAndRefusesBusyQueue(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	liveRoutes := caddyConfig + "\nbilling.apps.example.com {\n\treverse_proxy 127.0.0.1:12001\n}\n"
	if err := os.WriteFile(m.path("/etc/caddy/Caddyfile"), []byte(liveRoutes), 0644); err != nil {
		t.Fatal(err)
	}
	customPrincipals := "[{\"id\":\"ci-billing\",\"role\":\"deployer\",\"apps\":[\"billing\"]}]\n"
	if err := os.WriteFile(m.path("/etc/vamos/principals.json"), []byte(customPrincipals), 0600); err != nil {
		t.Fatal(err)
	}
	m.Executor = []byte("executor-v2")
	m.Agent = []byte("agent-v2")
	if _, err := m.Upgrade(context.Background(), testConfig); err != nil {
		t.Fatalf("upgrade rejected live Caddy routes or custom ACLs: %v", err)
	}

	// An active or unresolved queue operation blocks upgrade without overwriting binaries.
	opsDir := m.path("/var/lib/vamos/queue/operations")
	if err := os.MkdirAll(opsDir, 0700); err != nil {
		t.Fatal(err)
	}
	runningRecord := `{"version":1,"operation_id":"0123456789abcdef0123456789abcdef","phase":"running"}`
	if err := os.WriteFile(filepath.Join(opsDir, "0123456789abcdef0123456789abcdef.json"), []byte(runningRecord), 0600); err != nil {
		t.Fatal(err)
	}
	m.Executor = []byte("executor-v3")
	m.Agent = []byte("agent-v3")
	if _, err := m.Upgrade(context.Background(), testConfig); err == nil {
		t.Fatal("upgrade succeeded while queue had a running operation")
	}
}

func TestUpgradeMigratesLegacyCaddyfileAndReloadsLiveConfig(t *testing.T) {
	const routeSuffix = "\nbilling.apps.example.com {\n\tcoraza_waf {\n\t\tload_owasp_crs\n\t}\n\tcache\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:12001\n}\n"
	for name, legacyPrefix := range map[string]string{
		"v0.3.0": LegacyBaseCaddyfileV030,
		"v0.2.0": LegacyBaseCaddyfileV020,
	} {
		t.Run(name, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.path(caddyfilePath), []byte(legacyPrefix), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil || !strings.Contains(err.Error(), "server upgrade") {
				t.Fatalf("repeat bootstrap accepted outdated Caddyfile without upgrade: %v", err)
			}
			legacyCaddy := legacyPrefix + routeSuffix
			if err := os.WriteFile(m.path(caddyfilePath), []byte(legacyCaddy), 0644); err != nil {
				t.Fatal(err)
			}
			before := len(f.calls)
			m.Executor = []byte("executor-" + name)
			m.Agent = []byte("agent-" + name)
			if _, err := m.Upgrade(context.Background(), testConfig); err != nil {
				t.Fatalf("upgrade from %s Caddyfile failed: %v", name, err)
			}
			got, err := os.ReadFile(m.path(caddyfilePath))
			if err != nil {
				t.Fatal(err)
			}
			want := caddyConfig + routeSuffix
			if string(got) != want {
				t.Fatalf("migrated Caddyfile = %q, want %q", got, want)
			}
			if indexOf(f.calls[before:], "docker exec "+CaddyContainer+" caddy reload --config "+caddyfilePath+" --adapter caddyfile") < 0 {
				t.Fatalf("upgrade did not reload running Caddy container after migrating Caddyfile: %v", f.calls[before:])
			}
		})
	}
}

func TestUpgradeRollsBackMigratedCaddyfileOnFailure(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	const routeSuffix = "\nbilling.apps.example.com {\n\treverse_proxy 127.0.0.1:12001\n}\n"
	legacyCaddy := LegacyBaseCaddyfileV030 + routeSuffix
	if err := os.WriteFile(m.path(caddyfilePath), []byte(legacyCaddy), 0644); err != nil {
		t.Fatal(err)
	}
	baseRun := m.Run
	m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "daemon-reload" {
			return process.Result{}, os.ErrPermission
		}
		return baseRun(ctx, name, args)
	}
	m.Executor = []byte("executor-fail")
	m.Agent = []byte("agent-fail")
	if _, err := m.Upgrade(context.Background(), testConfig); err == nil {
		t.Fatal("upgrade succeeded despite daemon-reload failure")
	}
	got, err := os.ReadFile(m.path(caddyfilePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != legacyCaddy {
		t.Fatalf("failed upgrade did not restore legacy Caddyfile: got %q, want %q", got, legacyCaddy)
	}
}
