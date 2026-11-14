package buildkit

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingIntentDoesNotAdoptWrongNonceOrShape(t *testing.T) {
	for _, kind := range []string{"nonce", "env", "bind", "runtime", "ports", "command", "endpoint"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			// Remove only the captured container identity to model a crash immediately
			// after creation. All prerequisites were persisted before docker create.
			s := f.state()
			s.ContainerID = ""
			s.ContainerConfig = nil
			s.ContainerHost = nil
			if save(filepath.Join(f.root, "var/lib/soar/buildkit"), s) != nil {
				t.Fatal("save")
			}
			cfg := f.container["Config"].(map[string]any)
			host := f.container["HostConfig"].(map[string]any)
			switch kind {
			case "nonce":
				cfg["Labels"] = map[string]string{"org.soar.buildkit.host": testHost, "org.soar.buildkit.nonce": strings.Repeat("0", 32), "org.soar.buildkit.role": "container"}
			case "env":
				cfg["Env"] = []string{"HTTP_PROXY=unowned"}
			case "bind":
				host["Binds"] = []string{"/etc:/etc"}
			case "runtime":
				host["Runtime"] = "foreign"
			case "ports":
				host["PortBindings"] = map[string]any{"1234/tcp": []any{map[string]any{"HostIp": "0.0.0.0", "HostPort": "1234"}}}
			case "command":
				cfg["Cmd"] = []string{"--allow-insecure-entitlement=network.host"}
			case "endpoint":
				f.container["NetworkSettings"] = map[string]any{"Networks": map[string]any{"foreign": map[string]any{"NetworkID": strings.Repeat("d", 64)}}}
			}
			failBuild(t, m, f)
			for _, call := range f.calls {
				if strings.HasPrefix(call, "container stop") || strings.HasPrefix(call, "container kill") {
					t.Fatal("stopped unverified container")
				}
			}
		})
	}
}
