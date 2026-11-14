// Package buildkit manages one receipt-owned, network-isolated BuildKit daemon
// on the local worker. The caller MUST hold the global worker lock for Build.
// Root and the Docker daemon are trusted; this is not a hostile-tenant sandbox.
package buildkit

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/nchapman/soar/internal/process"
)

type Runner func(context.Context, string, []string, []byte) (process.Result, error)

type Manager struct {
	// Root rebases private on-disk metadata only, not the Docker socket or commands.
	Root string
	Run  Runner
	// Snapshotter is auto (default) or native. Empty reuses the receipt's choice;
	// an explicit different choice is rejected, never silently migrated.
	Snapshotter string
}

type build struct {
	m           Manager
	s           receipt
	dir, stopID string
}

// Build loads an image into the local Docker daemon. Only build output is
// returned; infrastructure subprocess diagnostics and errors are value-free.
// Cancellation never cancels the bounded independent stop attempt.
func (m Manager) Build(ctx context.Context, hostID, dockerfile, iidfile string, archive []byte) (result process.Result, err error) {
	s, dir, err := m.state(hostID)
	if err != nil {
		return result, errInfra
	}
	b := &build{m: m, s: s, dir: dir}
	defer func() {
		if b.stopID != "" && b.stop() != nil {
			err = errors.Join(err, errInfra)
		}
	}()
	// A captured immutable ID also permits independently validated cleanup if
	// the caller is already canceled or the name inventory is unavailable.
	b.stopID = s.ContainerID
	// Stop a verified leftover before any other inventory or firewall repair.
	if b.existingContainer(ctx) != nil {
		return result, errInfra
	}
	if b.network(ctx) != nil || b.volume(ctx) != nil || b.image(ctx) != nil || b.createContainer(ctx) != nil {
		return result, errInfra
	}
	if b.builder(ctx) != nil || b.firewall(ctx) != nil {
		return result, errInfra
	}
	if _, e := b.docker(ctx, "container", "start", b.stopID); e != nil {
		return result, errInfra
	}
	// Recheck actual attachment after Docker has allocated networking.
	var c containerInfo
	if b.inspect(ctx, "container", b.stopID, &c) != nil || b.validateContainer(c, b.stopID) != nil || !c.State.Running || c.State.Paused || c.State.Restarting || b.network(ctx) != nil {
		return result, errInfra
	}
	result, e := b.dockerInput(ctx, archive, "buildx", "build", "--builder", s.Builder, "--load", "--network", "default", "--iidfile", iidfile, "--file", dockerfile, "-")
	if e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("buildkit build failed")
	}
	return result, nil
}
func (b *build) command(ctx context.Context, exe string, args []string, stdin []byte) (process.Result, error) {
	run := b.m.Run
	if run == nil {
		run = process.Run
	}
	// Root's registry configuration and credential-helper environment remain
	// trusted, as before. Constrain endpoint/driver selection without copying
	// credentials or mounting them inside the builder.
	env := []string{"-u", "DOCKER_CONTEXT", "-u", "DOCKER_TLS", "-u", "DOCKER_TLS_VERIFY", "-u", "DOCKER_CERT_PATH", "-u", "BUILDKIT_HOST", "-u", "BUILDX_BUILDER", "DOCKER_HOST=unix:///var/run/docker.sock", "BUILDX_CONFIG=" + filepath.Join(b.dir, "buildx"), exe}
	return run(ctx, "/usr/bin/env", append(env, args...), stdin)
}
func (b *build) docker(ctx context.Context, args ...string) (process.Result, error) {
	return b.dockerInput(ctx, nil, args...)
}
func (b *build) dockerInput(ctx context.Context, stdin []byte, args ...string) (process.Result, error) {
	return b.command(ctx, "docker", append([]string{"--host", "unix:///var/run/docker.sock"}, args...), stdin)
}
func (b *build) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var c containerInfo
	if b.inspect(ctx, "container", b.stopID, &c) != nil || b.validateContainer(c, b.stopID) != nil {
		return errInfra
	}
	if c.State.Running || c.State.Paused || c.State.Restarting {
		if _, err := b.docker(ctx, "container", "stop", "--time", "5", b.stopID); err != nil {
			// A failed stop is ambiguous, and can leave the process alive. Kill only
			// the same validated immutable ID, never a name or a replacement.
			if _, err = b.docker(ctx, "container", "kill", b.stopID); err != nil {
				return errInfra
			}
		}
	}
	if b.inspect(ctx, "container", b.stopID, &c) != nil || b.validateContainer(c, b.stopID) != nil || c.State.Running || c.State.Paused || c.State.Restarting {
		return errInfra
	}
	return nil
}
