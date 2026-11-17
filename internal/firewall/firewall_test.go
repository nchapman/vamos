package firewall

import (
	"strings"
	"testing"
)

// Regression: never permit host INPUT application ports or bypass Docker isolation.
func TestRulesBoundaries(t *testing.T) {
	p := plan{SSHPorts: []int{2222}, Bridges: []string{"br-+", "vamos-build0"}}
	for _, family := range []string{"iptables", "ip6tables"} {
		rules := p.rules(family)
		text := strings.Join(rules, "\n")
		for _, want := range []string{"--dport 2222", "--dport 443", "-j DROP"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %s", family, want)
			}
		}
		if family == "iptables" {
			for _, want := range []string{"-i br-+ -j RETURN", "-i vamos-build0 -j RETURN", "--ctstate DNAT --ctdir ORIGINAL", "-p tcp", "-p udp"} {
				if !strings.Contains(text, want) {
					t.Fatal(want)
				}
			}
		} else if strings.Contains(text, "-j RETURN") {
			t.Fatal("IPv6 forwarding allowed")
		}
		if strings.Contains(text, "docker0") {
			t.Fatal("default docker bridge allowed")
		}
		if strings.Contains(text, "-F INPUT") || strings.Contains(text, "-P FORWARD") || strings.Contains(text, "-i docker0 -j ACCEPT") {
			t.Fatal("unsafe ownership/isolation")
		}
		if family == "ip6tables" && !strings.Contains(text, "::1/128") {
			t.Fatal("IPv6 loopback publish not excluded")
		}
	}
}

func TestForeignPolicyRejected(t *testing.T) {
	for _, text := range []string{
		"*filter\n:INPUT DROP [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n",
		"*filter\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n-A INPUT -p tcp --dport 22 -j ACCEPT\nCOMMIT\n",
		"*filter\n:CUSTOM - [0:0]\nCOMMIT\n",
	} {
		if err := validateBaseline(text); err == nil {
			t.Fatalf("accepted foreign policy: %s", text)
		}
	}
}

func TestSSHConnection(t *testing.T) {
	if port, err := connectionPort("203.0.113.4 50000 10.0.0.1 2222"); err != nil || port != 2222 {
		t.Fatalf("host-side NAT port: %v %v", port, err)
	}
	for _, s := range []string{"", "a 1 b 22", "1.2.3.4 1 1.2.3.5 0", "1.2.3.4 1 1.2.3.5 22;id"} {
		if _, err := connectionPort(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
