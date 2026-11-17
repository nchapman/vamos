package firewall

import (
	"fmt"
	"strconv"
	"strings"
)

const inputChain = "VAMOS-FW-IN"
const forwardChain = "VAMOS-FW-FWD"

type plan struct {
	SSHPorts []int    `json:"ssh_ports"`
	Bridges  []string `json:"bridges"`
}

// rules are fixed tokens plus validated integer ports/interface names. RETURN,
// never ACCEPT, leaves Docker's own isolation and publish policy authoritative.
func (p plan) rules(family string) []string {
	in, fw := "-A "+inputChain+" ", "-A "+forwardChain+" "
	loopback := "127.0.0.0/8"
	if family == "ip6tables" {
		loopback = "::1/128"
	}
	r := []string{in + "-m conntrack --ctstate INVALID -j DROP", in + "! -i lo -d " + loopback + " -j DROP", in + "-i lo -j ACCEPT", in + "-m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"}
	if family == "iptables" {
		r = append(r, in+"-d 169.254.0.0/16 -j DROP", in+"-p icmp -j ACCEPT", in+"-p udp --sport 67 --dport 68 -j ACCEPT")
	} else {
		// ICMPv6 includes PMTU and neighbor/router discovery required by IPv6.
		r = append(r, in+"-p ipv6-icmp -j ACCEPT", in+"-s fe80::/10 -p udp --sport 547 --dport 546 -j ACCEPT")
	}
	for _, port := range p.SSHPorts {
		r = append(r, fmt.Sprintf("%s! -i br-+ -p tcp --dport %d -j ACCEPT", in, port))
	}
	r = append(r, in+"-p tcp --dport 80 -j ACCEPT", in+"-p tcp --dport 443 -j ACCEPT", in+"-p udp --dport 443 -j ACCEPT", in+"-j DROP")
	r = append(r, fw+"-m conntrack --ctstate INVALID -j DROP")
	// Application IPv6 networking is unsupported; host IPv6 remains usable.
	if family == "ip6tables" {
		return append(r, fw+"-j DROP")
	}
	r = append(r, fw+"-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")
	for _, b := range p.Bridges {
		r = append(r, fw+"-i "+b+" -d 169.254.0.0/16 -j DROP", fw+"-i "+b+" -j RETURN")
	}
	for _, b := range p.Bridges {
		for _, protocol := range []string{"tcp", "udp"} {
			r = append(r, fw+"-o "+b+" -p "+protocol+" -m conntrack --ctstate DNAT --ctdir ORIGINAL ! --ctorigdst "+loopback+" -j RETURN")
		}
	}
	return append(r, fw+"-j DROP")
}

var dockerChains = map[string]bool{
	"DOCKER": true, "DOCKER-USER": true, "DOCKER-FORWARD": true,
	"DOCKER-BRIDGE": true, "DOCKER-CT": true, "DOCKER-INTERNAL": true,
	"DOCKER-ISOLATION-STAGE-1": true, "DOCKER-ISOLATION-STAGE-2": true,
}

// Deliberately conservative: only pristine builtins and Docker's own chains.
// Rules in Docker-owned chains are trusted only after Docker/network inspection.
func validateBaseline(text string) error {
	table := ""
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") || line == "COMMIT" {
			continue
		}
		if strings.HasPrefix(line, "*") {
			table = line[1:]
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return fmt.Errorf("unrecognized firewall line %q", line)
		}
		if strings.HasPrefix(f[0], ":") {
			name := f[0][1:]
			if (table == "filter" && dockerChains[name]) || (table == "nat" && name == "DOCKER") {
				continue
			}
			switch name {
			case "INPUT", "OUTPUT", "FORWARD", "PREROUTING", "POSTROUTING":
				if f[1] != "ACCEPT" && !(table == "filter" && name == "FORWARD" && f[1] == "DROP") {
					return fmt.Errorf("foreign %s policy", name)
				}
			default:
				return fmt.Errorf("foreign chain %s", name)
			}
			continue
		}
		if f[0] != "-A" {
			return fmt.Errorf("unsupported firewall rule %q", line)
		}
		if (table == "filter" && dockerChains[f[1]]) || (table == "nat" && f[1] == "DOCKER") {
			if f[1] == "DOCKER-USER" && line != "-A DOCKER-USER -j RETURN" {
				return fmt.Errorf("foreign DOCKER-USER rule")
			}
			continue
		}
		// No host rules, custom NAT, raw/mangle, or administrator routing exceptions.
		target := ""
		for i := 2; i+1 < len(f); i++ {
			if f[i] == "-j" {
				target = f[i+1]
			}
		}
		if table == "filter" && (line == "-A FORWARD -j DOCKER-USER" || line == "-A FORWARD -j DOCKER-FORWARD" || line == "-A FORWARD -j DOCKER-ISOLATION-STAGE-1") {
			continue
		}
		if table == "nat" && (line == "-A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER" || line == "-A OUTPUT ! -d 127.0.0.0/8 -m addrtype --dst-type LOCAL -j DOCKER" || line == "-A OUTPUT ! -d ::1/128 -m addrtype --dst-type LOCAL -j DOCKER") {
			continue
		}
		// Docker bridge egress masquerade; network options are separately checked.
		if table == "raw" && f[1] == "PREROUTING" && target == "DROP" {
			continue
		}
		if table == "nat" && f[1] == "POSTROUTING" && target == "MASQUERADE" {
			continue
		}
		return fmt.Errorf("foreign/unsupported firewall rule %q", line)
	}
	return nil
}

func validDockerRaw(line string, endpoints map[string]string) bool {
	f := strings.Fields(line)
	if len(f) != 9 && len(f) != 15 {
		return false
	}
	if f[0] != "-A" || f[1] != "PREROUTING" || f[2] != "-d" || endpoints[f[3]] == "" || f[4] != "!" || f[5] != "-i" || endpoints[f[3]] != f[6] || f[len(f)-2] != "-j" || f[len(f)-1] != "DROP" {
		return false
	}
	if len(f) == 15 {
		p, e := strconv.Atoi(f[12])
		if e != nil || p < 1 || p > 65535 || f[7] != "-p" || (f[8] != "tcp" && f[8] != "udp") || f[9] != "-m" || f[10] != f[8] || f[11] != "--dport" {
			return false
		}
	}
	return true
}
