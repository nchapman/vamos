package lifecycle

import (
	"context"
	"errors"
	"github.com/nchapman/vamos/internal/process"
	"os"
	"strings"
	"testing"
)

func TestFirewallEnableAcceptsLiveRoutesButRejectsChangedBase(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		accepted     bool
	}{
		{"live routes", BaseCaddyfile + "app.example.com {\n reverse_proxy 127.0.0.1:31000\n}\n", true},
		{"changed base", "{ admin :2019 }\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.path(CaddyfilePath), []byte(tc.config), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.path("/etc/os-release"), []byte("ID=ubuntu\n"), 0644); err != nil {
				t.Fatal(err)
			}
			probe := errors.New("firewall admission reached")
			m.Run = func(context.Context, string, []string) (process.Result, error) { return process.Result{}, probe }
			_, err := m.Firewall(context.Background(), testConfig, "enable", "")
			if errors.Is(err, probe) != tc.accepted {
				t.Fatalf("admission accepted=%t, error=%v", tc.accepted, err)
			}
			if !tc.accepted && (err == nil || !strings.Contains(err.Error(), "Caddyfile")) {
				t.Fatalf("expected Caddy ownership rejection: %v", err)
			}
		})
	}
}

func TestFirewallDropinDirectoryAdmission(t *testing.T) {
	m, _ := newTestManager(t)
	dir := m.path("/etc/systemd/system/docker.service.d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.checkOverrides("docker.service"); err != nil {
		t.Fatalf("empty directory is not an override: %v", err)
	}
	for _, name := range []string{"foreign.conf", "50-vamos-firewall.conf"} {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte("[Unit]\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := m.checkOverrides("docker.service"); err == nil {
			t.Fatalf("accepted unowned override %s", name)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFirewallRequiresMatchingReadyInstallation(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Firewall(context.Background(), testConfig, "enable", ""); err == nil {
		t.Fatal("accepted missing ownership receipt")
	}
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	other := testConfig
	other.Name = "other"
	if _, err := m.Firewall(context.Background(), other, "enable", ""); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong identity: %v", err)
	}
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		t.Fatal(err)
	}
	s.Phase, s.Step = "installing", "configure"
	if err = m.save(s); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Firewall(context.Background(), testConfig, "enable", ""); err == nil {
		t.Fatal("accepted incomplete installation")
	}
}

func TestFirewallRejectsUnexpectedOperationAndToken(t *testing.T) {
	m, _ := newTestManager(t)
	for _, tc := range []struct{ op, token string }{{"flush", ""}, {"enable", "unexpected"}, {"disable", "unexpected"}, {"status", "unexpected"}, {"confirm", ""}} {
		if _, err := m.Firewall(context.Background(), testConfig, tc.op, tc.token); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
