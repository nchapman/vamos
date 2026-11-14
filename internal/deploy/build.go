package deploy

import (
	"context"
	"os"

	"github.com/nchapman/soar/internal/buildkit"
	"github.com/nchapman/soar/internal/process"
)

// dockerCommandArgs recognizes the build backend's endpoint-constrained wrapper
// so build failures still capture a bounded tail, not the beginning of a log.
func dockerCommandArgs(name string, args []string) []string {
	if name == "/usr/bin/env" {
		for i, arg := range args {
			if arg == "docker" || arg == "/usr/bin/docker" {
				name, args = "docker", args[i+1:]
				break
			}
		}
	}
	if name != "docker" {
		return nil
	}
	if len(args) >= 2 && args[0] == "--host" {
		args = args[2:]
	}
	return args
}

// ImageBuilder is the external build-service boundary. Runtime environment and
// secret values are deliberately absent from its input.
type ImageBuilder interface {
	Build(context.Context, string, string, string, []byte) (process.Result, error)
}

func (m *Manager) buildImage(ctx context.Context, hostID, dockerfile, iidfile string, archive []byte) (process.Result, error) {
	// In particular, live restore must not keep a builder running across a
	// daemon restart while its network guards are unavailable.
	if err := m.verifyMultiHost(); err != nil {
		return process.Result{}, err
	}
	builder := m.Builder
	if builder == nil {
		builder = &buildkit.Manager{
			Root: m.Root,
			Run:  buildkit.Runner(m.Run),
			// Privileged operator configuration, not an application option.
			// Native is useful on isolated hosts that cannot mount nested overlays.
			Snapshotter: os.Getenv("SOAR_BUILDKIT_SNAPSHOTTER"),
		}
	}
	return builder.Build(ctx, hostID, dockerfile, iidfile, archive)
}
