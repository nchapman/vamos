package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/engine"
	"github.com/nchapman/vamos/internal/process"
)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Use controlled, secret-free runtime errors from real Docker, not fixtures
// that assume Docker's version-specific wording. All resources are guest-only.
func runtimeFailures(ctx context.Context, target, image, network string) error {
	if image == "" {
		return fmt.Errorf("missing fixture image")
	}
	name := fmt.Sprintf("vamos-integration-port-%d", time.Now().UnixNano())
	run := func(command string) (process.Result, error) {
		return process.Run(ctx, "ssh", []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "--", target, "sudo -n -- " + command}, nil)
	}
	// Docker can DNAT a port owned by a host process when userland-proxy is
	// disabled. Use Docker's own allocated port as the conflict, not Caddy's.
	// Explicitly join the app network: Vamos disables Docker's default bridge.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = process.Run(cleanup, "ssh", []string{"-T", "-o", "BatchMode=yes", "--", target, "sudo -n -- docker rm -f " + quote(name) + " " + quote(name+"-holder")}, nil)
	}()
	if result, err := run("docker run --name " + quote(name+"-holder") + " --network " + quote(network) + " --pull=never -d -p 127.0.0.1:30444:8080 " + quote(image) + " python -m http.server 8080"); err != nil {
		return fmt.Errorf("start controlled port holder: %w: %s", err, result.Stderr)
	}
	for _, probe := range []struct{ command, code string }{
		{"docker run --name " + quote(name) + " --network " + quote(network) + " --pull=never -d -p 127.0.0.1:30444:8080 " + quote(image) + " python -m http.server 8080", "docker_port_bind"},
		{"docker run --pull=never --rm vamos-integration-nonexistent:" + name, "docker_image_access"},
	} {
		result, err := run(probe.command)
		if err == nil {
			return fmt.Errorf("expected runtime failure for %s", probe.code)
		}
		got := engine.RuntimeErrorCode(engine.ClassifyRuntimeFailure(result.Stderr))
		if got != probe.code {
			return fmt.Errorf("runtime classification=%q want=%q; controlled diagnostic=%q", got, probe.code, result.Stderr)
		}
	}
	return nil
}
