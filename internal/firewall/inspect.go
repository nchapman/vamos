package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func connectionPort(s string) (int, error) {
	f := strings.Fields(s)
	if len(f) != 4 || net.ParseIP(f[0]) == nil || net.ParseIP(f[2]) == nil {
		return 0, fmt.Errorf("a valid SSH_CONNECTION is required")
	}
	for _, x := range []string{f[1], f[3]} {
		n, e := strconv.Atoi(x)
		if e != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("invalid SSH_CONNECTION port")
		}
	}
	return strconv.Atoi(f[3])
}

var bridgeName = regexp.MustCompile(`^(br-\+|vamos-build0)$`)
var networkID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type network struct {
	ID         string `json:"Id"`
	Name       string
	Driver     string
	EnableIPv6 bool
	Labels     map[string]string
	Internal   bool
	Ingress    bool
	Options    map[string]string
	Containers map[string]struct{ IPv4Address, IPv6Address string }
	IPAM       struct{ Config []struct{ Subnet string } }
}

func (m *Manager) inspect(ctx context.Context, hostID string) (plan, error) {
	var p plan
	release, err := os.ReadFile(m.path("/etc/os-release"))
	if err != nil || !strings.Contains("\n"+string(release), "\nID=ubuntu\n") {
		return p, fmt.Errorf("only dedicated Ubuntu hosts are supported")
	}
	for _, file := range []string{"/etc/ufw/ufw.conf", "/etc/firewalld/firewalld.conf", "/etc/nftables.conf", "/etc/iptables/rules.v4", "/etc/iptables/rules.v6"} {
		if _, err := os.Stat(m.path(file)); err == nil {
			return p, fmt.Errorf("conflicting firewall configuration %s; use user-managed mode or explicitly remove the conflicting configuration first", file)
		} else if !os.IsNotExist(err) {
			return p, err
		}
	}
	for _, unit := range []string{"ufw.service", "firewalld.service", "nftables.service", "netfilter-persistent.service"} {
		out, err := m.command(ctx, "systemctl", "show", unit, "--property=LoadState,ActiveState,UnitFileState")
		if err != nil {
			return p, err
		}
		if strings.Contains(out, "ActiveState=active") || strings.Contains(out, "UnitFileState=enabled") {
			return p, fmt.Errorf("conflicting manager %s", unit)
		}
	}
	for _, file := range []string{"/etc/docker/daemon.json"} {
		data, e := os.ReadFile(m.path(file))
		if e != nil && !os.IsNotExist(e) {
			return p, e
		}
		if e == nil {
			var cfg map[string]json.RawMessage
			if e = json.Unmarshal(data, &cfg); e != nil {
				return p, e
			}
			if raw, ok := cfg["bridge"]; ok && string(raw) != `"none"` {
				return p, fmt.Errorf("Docker bridge must be none")
			}
			for _, key := range []string{"iptables", "ip6tables", "firewall-backend", "bip", "fixed-cidr", "fixed-cidr-v6", "allow-direct-routing", "default-network-opts", "ip-forward-no-drop", "userland-proxy", "ip", "hosts"} {
				if _, ok := cfg[key]; ok {
					return p, fmt.Errorf("unsupported Docker daemon override %s", key)
				}
			}
		}
	}
	// Reject daemon command-line overrides and custom wrappers too.
	exec, err := m.command(ctx, "systemctl", "show", "docker.service", "--property=ExecStart", "--value")
	if err != nil {
		return p, err
	}
	start := strings.Index(exec, "argv[]=")
	if start < 0 {
		return p, fmt.Errorf("unrecognized Docker service ExecStart")
	}
	argv := strings.SplitN(exec[start+len("argv[]="):], " ;", 2)[0]
	if argv != "/usr/bin/dockerd" && argv != "/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock" {
		return p, fmt.Errorf("nondefault Docker service command line")
	}
	if !strings.Contains(exec, "/usr/bin/dockerd") {
		return p, fmt.Errorf("unsupported Docker service executable")
	}
	for _, flag := range []string{"--iptables", "--ip6tables", "--firewall-backend", "--bridge", "--bip", "--fixed-cidr", "--allow-direct-routing", "--config-file", "--default-network", "--ip=", "--ip-forward", "--userland-proxy"} {
		if strings.Contains(exec, flag) {
			return p, fmt.Errorf("unsupported Docker flag %s", flag)
		}
	}
	info, err := m.command(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return p, err
	}
	var di struct {
		Swarm           struct{ LocalNodeState string }
		FirewallBackend json.RawMessage
	}
	if err = json.Unmarshal([]byte(info), &di); err != nil {
		return p, err
	}
	if di.Swarm.LocalNodeState != "inactive" || !dockerIPTablesBackend(di.FirewallBackend) {
		return p, fmt.Errorf("Docker swarm/native nftables backend unsupported")
	}
	ids, err := m.command(ctx, "docker", "network", "ls", "--quiet", "--no-trunc")
	if err != nil {
		return p, err
	}
	fields := strings.Fields(ids)
	if len(fields) == 0 {
		return p, fmt.Errorf("no Docker networks")
	}
	for _, id := range fields {
		if !networkID.MatchString(id) {
			return p, fmt.Errorf("invalid network ID")
		}
	}
	data, err := m.command(ctx, "docker", append([]string{"network", "inspect"}, fields...)...)
	if err != nil {
		return p, err
	}
	var nets []network
	if err = json.Unmarshal([]byte(data), &nets); err != nil {
		return p, err
	}
	subnets := map[string]string{}
	approved := map[string]string{}
	endpoints := map[string]string{}
	for _, n := range nets {
		if (n.Driver == "host" && n.Name == "host") || (n.Driver == "null" && n.Name == "none") {
			continue
		}
		if n.Driver != "bridge" || n.Internal || n.Ingress || n.EnableIPv6 || !networkID.MatchString(n.ID) {
			return p, fmt.Errorf("unsupported network %s", n.Name)
		}
		b := "br-" + n.ID[:12]
		build := n.Name == "vamos-buildkit-"+hostID+"-net" && ownedBuildkit(n.Labels, hostID, "network")
		if build {
			b = "vamos-build0"
		} else if n.Labels["vamos.host"] != hostID || n.Labels["vamos.app"] == "" || n.Name != "vamos-"+n.Labels["vamos.app"] {
			return p, fmt.Errorf("foreign Docker network %s", n.Name)
		}
		approved[n.Name], approved[n.ID] = n.Labels["vamos.app"], n.Labels["vamos.app"]
		if build {
			approved[n.Name], approved[n.ID] = "@buildkit", "@buildkit"
		}
		for key, val := range n.Options {
			switch key {
			case "com.docker.network.bridge.default_bridge":
				if val != "true" || b != "docker0" {
					return p, fmt.Errorf("nondefault bridge")
				}
			case "com.docker.network.bridge.name":
				if val != b {
					return p, fmt.Errorf("custom bridge name")
				}
			case "com.docker.network.bridge.enable_icc":
				if val != "true" && !(build && val == "false") {
					return p, fmt.Errorf("unsupported ICC policy")
				}
			case "com.docker.network.bridge.enable_ip_masquerade":
				if val != "true" {
					return p, fmt.Errorf("nondefault Docker bridge policy")
				}
			case "com.docker.network.driver.mtu":
				if val != "1500" && val != "0" {
					return p, fmt.Errorf("custom bridge MTU")
				}
			case "com.docker.network.bridge.host_binding_ipv4":
				if val != "0.0.0.0" {
					return p, fmt.Errorf("custom default bind address")
				}
			default:
				return p, fmt.Errorf("unsupported network option %s", key)
			}
		}
		p.Bridges = append(p.Bridges, b)
		for _, c := range n.Containers {
			if c.IPv6Address != "" {
				return p, fmt.Errorf("IPv6 application endpoint unsupported")
			}
			for _, address := range []string{c.IPv4Address} {
				if address == "" {
					continue
				}
				ip, _, e := net.ParseCIDR(address)
				if e != nil {
					return p, e
				}
				suffix := "/32"
				if ip.To4() == nil {
					suffix = "/128"
				}
				endpoints[ip.String()+suffix] = b
			}
		}
		for _, c := range n.IPAM.Config {
			_, cidr, e := net.ParseCIDR(c.Subnet)
			if e != nil {
				return p, e
			}
			if cidr.IP.To4() == nil {
				return p, fmt.Errorf("IPv6 application subnet unsupported")
			}
			subnets[cidr.String()] = b
		}
	}
	rawBindings := map[string]bool{}
	if err = m.inspectContainers(ctx, hostID, approved, rawBindings); err != nil {
		return p, err
	}
	links, err := m.command(ctx, "ip", "-j", "-d", "link", "show")
	if err != nil {
		return p, err
	}
	var ls []struct {
		IfName   string `json:"ifname"`
		LinkInfo struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	if err = json.Unmarshal([]byte(links), &ls); err != nil {
		return p, err
	}
	external := 0
	for _, l := range ls {
		kind := l.LinkInfo.Kind
		if kind == "" && l.IfName != "lo" {
			external++
		}
		if kind == "bridge" {
			if !contains(p.Bridges, l.IfName) {
				return p, fmt.Errorf("unapproved bridge %s", l.IfName)
			}
			continue
		}
		if kind != "" && kind != "veth" {
			return p, fmt.Errorf("unsupported router/VPN/link %s (%s)", l.IfName, kind)
		}
		if strings.HasPrefix(l.IfName, "tun") || strings.HasPrefix(l.IfName, "tap") || strings.HasPrefix(l.IfName, "wg") {
			return p, fmt.Errorf("VPN interface unsupported")
		}
	}
	// Virtualized hosts may expose their uplink as a veth. The policy treats
	// every non-approved ingress interface identically; none becomes trusted
	// merely because no physical NIC was reported.
	if external > 1 {
		return p, fmt.Errorf("multiple physical uplinks are unsupported (found %d)", external)
	}
	for _, family := range []string{"-4", "-6"} {
		data, e := m.command(ctx, "ip", "-j", family, "rule", "show")
		if e != nil {
			return p, e
		}
		if e = validateRoutingRules(data); e != nil {
			return p, e
		}
		// Provider-specific host routes (metadata, DNS, virtual gateways) are
		// ordinary outbound routing, not evidence of a router. Do not modify
		// them: the forwarding gate independently denies unsolicited transit.
	}

	backend := ""
	saves := map[string]string{}
	for _, family := range []string{"iptables", "ip6tables"} {
		v, e := m.command(ctx, family, "--version")
		if e != nil {
			return p, e
		}
		b := "legacy"
		if strings.Contains(v, "nf_tables") {
			b = "nft"
		} else if !strings.Contains(v, "legacy") {
			return p, fmt.Errorf("unknown iptables backend")
		}
		if backend != "" && b != backend {
			return p, fmt.Errorf("mixed iptables backends")
		}
		backend = b
		saved, e := m.command(ctx, family+"-save")
		if e != nil {
			return p, e
		}
		key := "ip"
		if family == "ip6tables" {
			key = "ip6"
		}
		saves[key] = saved
		if e = validateHostBaseline(saved, hostID); e != nil {
			return p, e
		}
		// Docker's direct-routing raw drops are accepted only for exact endpoints
		// on inventory-approved bridges; no arbitrary raw/mangle policy.
		for _, line := range strings.Split(saved, "\n") {
			if strings.HasPrefix(line, "-A PREROUTING") && strings.HasSuffix(line, "-j DROP") {
				if !validDockerRaw(line, endpoints) && !rawBindings[line] {
					return p, fmt.Errorf("unsupported Docker raw rule %s", line)
				}
			}
		}
		// Only the normal Docker per-subnet egress MASQUERADE form is supported.
		for _, line := range strings.Split(saved, "\n") {
			if strings.Contains(line, "-A POSTROUTING") {
				f := strings.Fields(line)
				if len(f) != 9 || f[2] != "-s" || subnets[f[3]] != f[6] || f[4] != "!" || f[5] != "-o" || !contains(p.Bridges, f[6]) || f[7] != "-j" || f[8] != "MASQUERADE" {
					return p, fmt.Errorf("unsupported NAT rule %s", line)
				}
			}
		}
		opposite := "nft"
		if backend == "nft" {
			opposite = "legacy"
		}
		// Ubuntu's iptables package provides both alternatives; inability to inspect
		// either is not evidence that it is safe to proceed.
		other, e := m.command(ctx, family+"-"+opposite+"-save")
		if e != nil {
			return p, e
		}
		for _, line := range strings.Split(other, "\n") {
			if strings.HasPrefix(line, "-A ") || (strings.HasPrefix(line, ":") && !strings.Contains(line, " ACCEPT ")) {
				return p, fmt.Errorf("rules in inactive iptables backend")
			}
		}
	}
	tables, err := m.nft(ctx, false)
	if err != nil {
		return p, err
	}
	for _, line := range strings.Split(strings.TrimSpace(tables), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if backend != "nft" || len(f) != 3 || f[0] != "table" || (f[1] != "ip" && f[1] != "ip6") || (f[2] != "filter" && f[2] != "nat" && f[2] != "raw" && f[2] != "mangle") {
			return p, fmt.Errorf("native nftables table unsupported: %s", line)
		}
	}
	if backend == "nft" {
		data, e := m.nft(ctx, true)
		if e != nil {
			return p, e
		}
		if e = validateNFT(data, saves); e != nil {
			return p, e
		}
	}

	port, err := connectionPort(os.Getenv("SSH_CONNECTION"))
	if err != nil {
		return p, err
	}
	// ss sees actual listeners (including socket activation), unlike sshd -T's
	// configured port, which may differ from the running daemon.
	listeners, err := m.command(ctx, "ss", "-H", "-lntp")
	if err != nil {
		return p, err
	}
	config, e := m.command(ctx, "sshd", "-T")
	if e != nil {
		return p, e
	}
	configured := map[string]bool{}
	for _, line := range strings.Split(config, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "port" {
			configured[f[1]] = true
		}
	}

	ports := map[int]bool{}
	for _, line := range strings.Split(listeners, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}

		if !strings.Contains(line, `"sshd"`) && !strings.Contains(line, `"sshd-session"`) && !strings.Contains(line, `"systemd"`) {
			continue
		}
		_, ps, e := net.SplitHostPort(f[3])
		if e != nil {
			return p, fmt.Errorf("unrecognized ssh listener %q", f[3])
		}
		if !strings.Contains(line, `"sshd"`) && !strings.Contains(line, `"sshd-session"`) && !(strings.Contains(line, `"systemd"`) && configured[ps]) {
			continue
		}
		n, e := strconv.Atoi(ps)
		if e != nil || n < 1 || n > 65535 {
			return p, fmt.Errorf("invalid SSH listener")
		}
		ports[n] = true
	}
	// The authenticated connection's local port also covers systemd-owned SSH
	// sockets, without allowing every other systemd-owned listening socket.
	ports[port] = true
	for n := range ports {
		p.SSHPorts = append(p.SSHPorts, n)
	}
	sort.Ints(p.SSHPorts)
	p.Bridges = []string{"br-+", "vamos-build0"}
	return p, nil
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func validateRoutingRules(data string) error {
	var rules []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &rules); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range rules {
		for k := range r {
			switch k {
			case "priority", "src", "dst", "table", "protocol":
			default:
				return fmt.Errorf("custom policy routing key %s", k)
			}
		}
		var priority int
		var table string
		if err := json.Unmarshal(r["priority"], &priority); err != nil {
			return err
		}
		if err := json.Unmarshal(r["table"], &table); err != nil {
			return err
		}
		if !((priority == 0 && table == "local") || (priority == 32766 && table == "main") || (priority == 32767 && table == "default")) || seen[table] {
			return fmt.Errorf("custom routing policy")
		}
		for _, key := range []string{"src", "dst"} {
			if raw, ok := r[key]; ok {
				var match string
				if json.Unmarshal(raw, &match) != nil || match != "all" {
					return fmt.Errorf("custom routing selector")
				}
			}
		}
		seen[table] = true
	}
	if !seen["local"] || !seen["main"] {
		return fmt.Errorf("missing default routing policies")
	}
	return nil
}
