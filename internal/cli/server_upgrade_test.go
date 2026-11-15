package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

func TestServerUpgradeUsesExplicitRootLifecycleOperation(t *testing.T) {
	store := registry.New(t.TempDir())
	server := registry.Server{Name: "prod", Target: "root@prod", Domain: "apps.example.com"}
	if err := store.Put(server); err != nil {
		t.Fatal(err)
	}
	called := false
	deps := lifecycleDeps{
		Registry: func() (*registry.Store, error) { return store, nil },
		Status: func(context.Context, string) (*ssh.Status, error) {
			return &ssh.Status{State: &lifecycle.State{Phase: "ready", Config: lifecycle.Config{Name: server.Name, Domain: server.Domain}}}, nil
		},
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("bundle"), nil },
		Run: func(_ context.Context, target string, request executor.Request, artifact []byte) (*lifecycle.State, error) {
			called = target == server.Target && request.Operation == "upgrade" && request.Confirm && !request.Purge && request.Config != nil && *request.Config == (lifecycle.Config{Name: server.Name, Domain: server.Domain}) && string(artifact) == "bundle"
			return &lifecycle.State{Phase: "ready", Config: *request.Config}, nil
		},
	}
	var out, stderr bytes.Buffer
	if code := serverLifecycle(context.Background(), "upgrade", []string{"--yes", "--executor", "bundle", server.Name}, &out, &stderr, deps); code != 0 || !called {
		t.Fatalf("code=%d called=%t stderr=%s", code, called, &stderr)
	}

	deps.Status = func(context.Context, string) (*ssh.Status, error) { return nil, errors.New("agent still active") }
	if code := serverLifecycle(context.Background(), "upgrade", []string{"--yes", server.Name}, &out, &stderr, deps); code != 1 {
		t.Fatalf("unexpected code=%d", code)
	}
}

func TestServerStatusAndUpgradeDefaultToRegisteredDefaultServer(t *testing.T) {
	store := registry.New(t.TempDir())
	server := registry.Server{Name: "prod", Target: "root@prod", Domain: "apps.example.com"}
	if err := store.Put(server); err != nil {
		t.Fatal(err)
	}
	var statusTargets, upgradeTargets []string
	deps := lifecycleDeps{
		Registry: func() (*registry.Store, error) { return store, nil },
		Status: func(_ context.Context, target string) (*ssh.Status, error) {
			statusTargets = append(statusTargets, target)
			return &ssh.Status{State: &lifecycle.State{Phase: "ready", Step: "complete", Config: lifecycle.Config{Name: server.Name, Domain: server.Domain}}}, nil
		},
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("bundle"), nil },
		Run: func(_ context.Context, target string, request executor.Request, _ []byte) (*lifecycle.State, error) {
			upgradeTargets = append(upgradeTargets, target)
			return &lifecycle.State{Phase: "ready", Config: *request.Config}, nil
		},
	}
	var out, stderr bytes.Buffer
	if code := serverLifecycle(context.Background(), "status", nil, &out, &stderr, deps); code != 0 {
		t.Fatalf("default server status failed: code=%d stderr=%s", code, &stderr)
	}
	if code := serverLifecycle(context.Background(), "upgrade", []string{"--yes"}, &out, &stderr, deps); code != 0 {
		t.Fatalf("default server upgrade failed: code=%d stderr=%s", code, &stderr)
	}
	if len(statusTargets) != 2 || statusTargets[0] != "root@prod" || len(upgradeTargets) != 1 || upgradeTargets[0] != "root@prod" {
		t.Fatalf("statusTargets=%v upgradeTargets=%v", statusTargets, upgradeTargets)
	}
	if code := serverLifecycle(context.Background(), "teardown", []string{"--yes", "--purge"}, &out, &stderr, deps); code != 2 {
		t.Fatalf("teardown without explicit server name should be rejected, got code=%d", code)
	}
}
