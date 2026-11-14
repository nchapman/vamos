package buildkit

import (
	"context"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestDockerDefaultOOMNormalizationDoesNotStrandBuilder(t *testing.T) {
	m, f := newEngine(t)
	f.hook = func(_ context.Context, args []string) (process.Result, error, bool) {
		if len(args) >= 2 && args[0] == "container" && f.container != nil {
			h := f.container["HostConfig"].(map[string]any)
			if _, exists := h["OomKillDisable"]; !exists {
				h["OomKillDisable"] = false
			}
			if args[1] == "start" {
				// Docker 29 reports false at create, then null after first start.
				h["OomKillDisable"] = nil
			}
		}
		return process.Result{}, nil, false
	}
	runBuild(t, m)
	if running(f) {
		t.Fatal("builder left running after harmless daemon normalization")
	}
	runBuild(t, m)
}

func TestDockerEmptyDNSNormalizationAfterReboot(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	// Docker reports null at create, but an empty list after daemon restart.
	f.container["HostConfig"].(map[string]any)["Dns"] = []any{}
	runBuild(t, m)
	f.container["HostConfig"].(map[string]any)["Dns"] = []any{"192.0.2.1"}
	failBuild(t, m, f)
}

func TestRootDockerProxyDefaultsAreCapturedWithoutAllowingOtherEnvironment(t *testing.T) {
	m, f := newEngine(t)
	f.hook = func(_ context.Context, args []string) (process.Result, error, bool) {
		if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" && f.container != nil {
			cfg := f.container["Config"].(map[string]any)
			env := []string{"HTTP_PROXY=http://proxy.invalid:3128", "http_proxy=http://proxy.invalid:3128"}
			cfg["Env"] = append(env, f.state().ImageEnv...)
		}
		return process.Result{}, nil, false
	}
	runBuild(t, m)
	f.hook = nil
	runBuild(t, m)
	cfg := f.container["Config"].(map[string]any)
	cfg["Env"] = append(cfg["Env"].([]string), "UNEXPECTED=value")
	failBuild(t, m, f)
}

func TestChangedOOMPolicyIsStillRefused(t *testing.T) {
	m, f := newEngine(t)
	runBuild(t, m)
	f.container["HostConfig"].(map[string]any)["OomKillDisable"] = true
	failBuild(t, m, f)
}
