package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/firewall"
)

func TestFirewallFailureRecovery(t *testing.T) {
	for _, rich := range []bool{false, true} {
		for _, operation := range []string{"enable", "disable", "status"} {
			for _, artifact := range []string{"", "/tmp/executor file"} {
				var plain bytes.Buffer
				terminal := newHelpAppsTerminal(48, false)
				var out io.Writer = &plain
				if rich {
					out = terminal
				}
				if code := firewallFailure(out, operation, "home", "ubuntu@host", artifact, errors.New("controlled failure")); code != 1 {
					t.Fatalf("code=%d", code)
				}
				text := plain.String() + terminal.String()
				normalized := strings.Join(strings.Fields(text), " ")
				args := []string{"vamos", "server", "firewall", "status"}
				if artifact != "" {
					args = append(args, "--executor", artifact)
				}
				want := suggestedCommand(append(args, "home")...)
				for _, part := range []string{"controlled failure", want, "before retrying"} {
					if !strings.Contains(normalized, part) {
						t.Fatalf("missing %q: %s", part, text)
					}
				}
				if (operation == "disable") != strings.Contains(normalized, "no automatic rollback") {
					t.Fatal(text)
				}
				if operation == "disable" && !strings.Contains(normalized, "ownership or drift") {
					t.Fatal(text)
				}
			}
		}
	}
}

func TestFirewallStatusUsesTerminalPresentation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status firewall.Status
		want   string
	}{
		{"enabled", firewall.Status{Enabled: true, SSHPorts: []int{22, 2222}}, "Protection enabled"},
		{"disabled", firewall.Status{}, "Protection disabled"},
		{"pending", firewall.Status{Enabled: true, Pending: true}, "Confirmation pending"},
		{"degraded", firewall.Status{Enabled: true, Degraded: true, Problems: []string{"owned rule changed"}}, "Needs attention"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.status.Token = "PRIVATE_CONFIRMATION_TOKEN"
			out := newHelpAppsTerminal(48, false)
			if err := renderFirewallStatus(out, "home", &tt.status); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(strings.Fields(out.String()), " ")
			for _, want := range []string{"SERVER FIREWALL", "home", tt.want} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			if strings.Contains(text, "home /") {
				t.Fatal("empty target left a dangling header separator")
			}
			if strings.Contains(text, tt.status.Token) {
				t.Fatal("confirmation token leaked")
			}
			if tt.status.Pending && !strings.Contains(text, "rollback") {
				t.Fatal(text)
			}
			if tt.status.Degraded && !strings.Contains(text, "owned rule changed") {
				t.Fatal(text)
			}
		})
	}
}
