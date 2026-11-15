// Package buildkit manages one receipt-owned, network-isolated BuildKit daemon
// on the local worker. The caller MUST hold the global worker lock for Build.
// Root and the Docker daemon are trusted; this is not a hostile-tenant sandbox.
package buildkit

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/process"
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

func (m Manager) lock(ctx context.Context) (func(), error) {
	root := m.Root
	if root == "" {
		root = "/"
	}
	lockPath := filepath.Join(root, "run/lock/vamos/buildkit.lock")
	if safeDir(filepath.Dir(lockPath), true) != nil {
		return nil, errInfra
	}
	l := flock.New(lockPath, flock.SetPermissions(0600))
	lockCtx, cancelLock := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelLock()
	ok, err := l.TryLockContext(lockCtx, 25*time.Millisecond)
	if err != nil || !ok {
		_ = l.Close()
		return nil, errInfra
	}
	return func() {
		_ = l.Unlock()
		_ = l.Close()
	}, nil
}

// Build loads an image into the local Docker daemon. Only build output is
// returned; infrastructure subprocess diagnostics and errors are value-free.
// Cancellation never cancels the bounded independent stop attempt.
func (m Manager) Build(ctx context.Context, hostID, dockerfile, iidfile string, archive []byte) (result process.Result, err error) {
	unlock, lockErr := m.lock(ctx)
	if lockErr != nil {
		return result, lockErr
	}
	defer unlock()
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
	for attempt := 0; ; attempt++ {
		if err := b.start(ctx); err != nil {
			return result, err
		}
		result, e := b.dockerInput(ctx, archive, "buildx", "build", "--builder", s.Builder, "--load", "--network", "default", "--iidfile", iidfile, "--file", dockerfile, "-")
		if e == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !b.builderStopped(ctx) {
			return result, errors.New("buildkit build failed")
		}
		// buildkitd only exits when it is killed, typically by a Docker daemon
		// restart (live-restore is off). Build inputs are trusted and builds are
		// repeatable, so retry once through the full verified startup path.
		if attempt > 0 {
			return result, errors.New("buildkit build failed: builder stopped during build")
		}
	}
}

// Builder recovery waits out a restarting Docker daemon before deciding
// whether a failed build was an interruption. Variables for tests.
var (
	builderRecoveryPoll   = time.Second
	builderRecoveryWindow = time.Minute
)

// builderStopped reports whether the verified builder is no longer running
// after a failed build. An unavailable daemon is retried within the recovery
// window; a running builder means the build itself failed.
func (b *build) builderStopped(ctx context.Context) bool {
	deadline := time.Now().Add(builderRecoveryWindow)
	for {
		var c containerInfo
		if b.inspect(ctx, "container", b.stopID, &c) == nil {
			return b.validateContainer(c, b.stopID) == nil && !c.State.Running && !c.State.Paused && !c.State.Restarting
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(builderRecoveryPoll):
		}
	}
}

// start stops a verified leftover, repairs retained resources and guards, then
// starts the exact receipt-owned builder and rechecks its network attachment.
func (b *build) start(ctx context.Context) error {
	// Stop a verified leftover before any other inventory or firewall repair.
	if b.existingContainer(ctx) != nil {
		return errInfra
	}
	if b.network(ctx) != nil || b.volume(ctx) != nil || b.image(ctx) != nil || b.createContainer(ctx) != nil {
		return errInfra
	}
	if b.builder(ctx) != nil || b.firewall(ctx) != nil {
		return errInfra
	}
	if _, e := b.docker(ctx, "container", "start", b.stopID); e != nil {
		return errInfra
	}
	// Recheck actual attachment after Docker has allocated networking.
	var c containerInfo
	if b.inspect(ctx, "container", b.stopID, &c) != nil || b.validateContainer(c, b.stopID) != nil || !c.State.Running || c.State.Paused || c.State.Restarting || b.network(ctx) != nil {
		return errInfra
	}
	return nil
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
