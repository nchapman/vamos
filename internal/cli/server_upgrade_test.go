package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

func TestServerUpgradeDisclosesEffectsBeforeConsent(t *testing.T) {
	for _, args := range [][]string{{"help", "server"}, {"help", "server", "upgrade"}, {"server", "upgrade", "prod"}} {
		var out bytes.Buffer
		code := Run(args, &out, &out, "dev")
		if args[0] == "help" && code != 0 || args[0] == "server" && code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
		for _, want := range []string{"agent", "Caddy", "interrupt public traffic", "managed firewall"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v missing %q in %s", args, want, &out)
			}
		}
	}
}

func TestServerDisclosesAutomaticIngressPatchesBeforeConsent(t *testing.T) {
	for _, action := range []string{"add", "upgrade"} {
		for _, args := range [][]string{{"help", "server", action}, {"server", action, "prod"}} {
			var out bytes.Buffer
			Run(args, &out, &out, "dev")
			if !strings.Contains(out.String(), "daily Caddy patch") || !strings.Contains(out.String(), "interrupt public traffic") {
				t.Fatalf("%v: missing daily patch notice: %s", args, &out)
			}
		}
	}
}

func TestServerUpgradeUsesExplicitRootLifecycleOperation(t *testing.T) {
	store := registry.New(t.TempDir())
	server := registry.Server{Name: "prod", Target: "root@prod", Domain: "apps.example.com"}
	if err := store.Put(server); err != nil {
		t.Fatal(err)
	}
	called := false
	var out, stderr bytes.Buffer
	deps := lifecycleDeps{
		Registry: func() (*registry.Store, error) { return store, nil },
		Status: func(context.Context, string) (*ssh.Status, error) {
			return &ssh.Status{State: &lifecycle.State{Phase: "ready", CaddyContainerID: "old", Config: lifecycle.Config{Name: server.Name, Domain: server.Domain}}}, nil
		},
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("bundle"), nil },
		Run: func(_ context.Context, target string, request executor.Request, artifact []byte) (*lifecycle.State, error) {
			if !strings.Contains(out.String(), "interrupt public traffic") {
				t.Error("missing warning before remote upgrade")
			}
			called = target == server.Target && request.Operation == "upgrade" && request.Confirm && !request.Purge && request.Config != nil && *request.Config == (lifecycle.Config{Name: server.Name, Domain: server.Domain}) && string(artifact) == "bundle"
			return &lifecycle.State{Phase: "ready", CaddyContainerID: "new", Config: *request.Config}, nil
		},
	}
	if code := serverLifecycle(context.Background(), "upgrade", []string{"--yes", "--executor", "bundle", server.Name}, &out, &stderr, deps); code != 0 || !called {
		t.Fatalf("code=%d called=%t stderr=%s", code, called, &stderr)
	}

	if !strings.Contains(out.String(), "Caddy ingress was replaced") {
		t.Fatalf("upgrade did not report ingress replacement: %s", &out)
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
	if code := serverLifecycle(context.Background(), "teardown", []string{"--yes", "--purge", ""}, &out, &stderr, deps); code != 2 || len(statusTargets) != 2 || len(upgradeTargets) != 1 {
		t.Fatalf("empty teardown name selected registered default: code=%d status=%v mutation=%v", code, statusTargets, upgradeTargets)
	}
	if code := serverLifecycle(context.Background(), "teardown", []string{"--yes", "--purge"}, &out, &stderr, deps); code != 2 {
		t.Fatalf("teardown without explicit server name should be rejected, got code=%d", code)
	}
}
