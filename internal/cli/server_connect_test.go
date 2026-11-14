package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/registry"
	"github.com/nchapman/soar/internal/ssh"
)

func connectState() *lifecycle.State {
	return &lifecycle.State{Version: 1, ID: strings.Repeat("a", 32), Config: lifecycle.Config{Name: "home", Domain: "apps.example.com"}, Phase: "ready", Step: "complete", Configured: true, ExecutorSHA: strings.Repeat("b", 64), Accounts: map[string]string{"passwd:caddy": "owned", "group:caddy": "owned", "group:docker": "owned"}, Packages: []lifecycle.Package{{Name: "docker.io", Version: "1.0"}}}
}

func TestConnectExistingHostRegistersWithoutMutation(t *testing.T) {
	store := registry.New(t.TempDir())
	reads := 0
	deps := lifecycleDeps{
		Registry: func() (*registry.Store, error) { return store, nil },
		Inspect: func(ctx context.Context, target string) (*host.Report, error) {
			if target != "root@home" {
				t.Fatal(target)
			}
			return &host.Report{Root: true, Supported: true}, nil
		},
		Status: func(ctx context.Context, target string) (*ssh.Status, error) {
			reads++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("missing deadline")
			}
			return &ssh.Status{State: connectState()}, nil
		},
		// Nil artifact and mutation boundaries prove no bootstrap/executor upload.
	}
	for i := 0; i < 2; i++ {
		var out, diagnostic bytes.Buffer
		code := serverLifecycle(context.Background(), "connect", []string{"--domain", "apps.example.com", "home", "root@home"}, &out, &diagnostic, deps)
		if code != 0 {
			t.Fatalf("%d: %s", code, &diagnostic)
		}
		if !strings.Contains(out.String(), strings.Repeat("a", 32)) {
			t.Fatal("installation identity not reported")
		}
		got, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		want := registry.Server{Name: "home", Target: "root@home", Domain: "apps.example.com"}
		if len(got.Servers) != 1 || got.Servers["home"] != want || got.Default != "home" {
			t.Fatalf("unexpected registration: %+v", got)
		}
	}
	if reads != 2 {
		t.Fatal("repeat connect did not verify remote identity")
	}
}

func TestConnectExistingHostRefusesUnverifiedState(t *testing.T) {
	for _, kind := range []string{"missing", "active", "wrong-name", "wrong-domain", "installing", "removed", "invalid-id", "invalid-version", "incomplete", "unconfigured", "missing-ownership", "status-failure", "non-root", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			store := registry.New(t.TempDir())
			s := connectState()
			status := &ssh.Status{State: s}
			switch kind {
			case "missing":
				status.State = nil
			case "active":
				status.Active = true
			case "wrong-name":
				s.Config.Name = "other"
			case "wrong-domain":
				s.Config.Domain = "other.example.com"
			case "installing":
				s.Phase = "installing"
				s.Step = "configure"
			case "removed":
				s.Phase = "removed"
				s.PurgeApproved = true
			case "invalid-id":
				s.ID = strings.Repeat("z", 32)
			case "invalid-version":
				s.Version = 2
			case "incomplete":
				s.Step = "configure"
			case "unconfigured":
				s.Configured = false
			case "missing-ownership":
				s.Packages = nil
			}
			deps := lifecycleDeps{Registry: func() (*registry.Store, error) { return store, nil }, Inspect: func(context.Context, string) (*host.Report, error) {
				return &host.Report{Root: kind != "non-root", Supported: kind != "unsupported"}, nil
			}, Status: func(context.Context, string) (*ssh.Status, error) {
				if kind == "status-failure" {
					return nil, errors.New("connection failed")
				}
				return status, nil
			}}
			var out, diagnostic bytes.Buffer
			if code := serverLifecycle(context.Background(), "connect", []string{"--domain", "apps.example.com", "home", "root@home"}, &out, &diagnostic, deps); code != 1 {
				t.Fatalf("code %d: %s", code, &diagnostic)
			}
			got, err := store.Read()
			if err != nil || len(got.Servers) != 0 {
				t.Fatalf("registered unverified host: %+v %v", got, err)
			}
		})
	}
}

func TestConnectExistingHostRejectsConflictsBeforeContact(t *testing.T) {
	for _, registered := range []registry.Server{{Name: "home", Target: "root@other", Domain: "apps.example.com"}, {Name: "home", Target: "root@home", Domain: "other.example.com"}, {Name: "other", Target: "root@home", Domain: "apps.example.com"}} {
		store := registry.New(t.TempDir())
		if err := store.Put(registered); err != nil {
			t.Fatal(err)
		}
		deps := lifecycleDeps{Registry: func() (*registry.Store, error) { return store, nil }}
		var out, diagnostic bytes.Buffer
		if code := serverLifecycle(context.Background(), "connect", []string{"--domain", "apps.example.com", "home", "root@home"}, &out, &diagnostic, deps); code != 2 {
			t.Fatalf("code %d: %s", code, &diagnostic)
		}
	}
}
