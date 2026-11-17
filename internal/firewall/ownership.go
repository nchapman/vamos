package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var hostIdentity = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ownedBuildkit(labels map[string]string, host, role string) bool {
	return hostIdentity.MatchString(host) && labels["org.vamos.buildkit.host"] == host && labels["org.vamos.buildkit.role"] == role && hostIdentity.MatchString(labels["org.vamos.buildkit.nonce"])
}

func buildkitGuards(host string) []string {
	if !hostIdentity.MatchString(host) {
		return nil
	}
	comment := ` -m comment --comment "vamos-buildkit:` + host + `" -j DROP`
	return []string{
		"-A PREROUTING -i vamos-build0 -m addrtype --dst-type LOCAL" + comment,
		"-A FORWARD -i vamos-build0 -o br-+" + comment,
		"-A FORWARD -i vamos-build0 -o docker0" + comment,
		"-A FORWARD -i vamos-build0 -o vamos-build0" + comment,
	}
}

func validateHostBaseline(text, host string) error {
	table := ""
	var clean []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "*") {
			table = line[1:]
		}
		if table == "mangle" && contains(buildkitGuards(host), line) {
			continue
		}
		clean = append(clean, line)
	}
	return validateBaseline(strings.Join(clean, "\n"))
}

// Admission inspects stopped containers too: restarting one must not introduce
// an unreviewed host-network or foreign bridge workload. Docker admins are trusted.
func (m *Manager) inspectContainers(ctx context.Context, host string, networks map[string]string, rawBindings map[string]bool) error {
	ids, err := m.command(ctx, "docker", "container", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return err
	}
	fields := strings.Fields(ids)
	if len(fields) == 0 {
		return nil
	}
	for _, id := range fields {
		if !networkID.MatchString(id) {
			return fmt.Errorf("invalid container ID")
		}
	}
	data, err := m.command(ctx, "docker", append([]string{"container", "inspect"}, fields...)...)
	if err != nil {
		return err
	}
	var containers []struct {
		Name       string
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			NetworkMode string
			Privileged  bool
		}
		NetworkSettings struct {
			Networks map[string]json.RawMessage
			Ports    map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string
			}
		}
	}
	if err := json.Unmarshal([]byte(data), &containers); err != nil {
		return err
	}
	for _, c := range containers {
		labels, mode := c.Config.Labels, c.HostConfig.NetworkMode
		if labels["org.vamos.caddy.host"] == host && c.Name == "/vamos-caddy" && mode == "host" && !c.HostConfig.Privileged {
			continue
		}
		app, ok := networks[mode]
		build := ownedBuildkit(labels, host, "container") && c.Name == "/vamos-buildkit-"+host
		if !ok || len(c.NetworkSettings.Networks) > 1 || (!build && (c.HostConfig.Privileged || labels["vamos.host"] != host || labels["vamos.app"] == "" || labels["vamos.app"] != app)) || (build && app != "@buildkit") {
			return fmt.Errorf("foreign/unsupported Docker container %s (network %s)", c.Name, mode)
		}
		for port, bindings := range c.NetworkSettings.Ports {
			parts := strings.Split(port, "/")
			if len(parts) != 2 || (parts[1] != "tcp" && parts[1] != "udp") {
				continue
			}
			for _, bind := range bindings {
				ip := net.ParseIP(bind.HostIP)
				number, e := strconv.Atoi(bind.HostPort)
				if ip == nil || ip.To4() == nil || !ip.IsLoopback() || e != nil || number < 1 || number > 65535 {
					continue
				}
				rawBindings[fmt.Sprintf("-A PREROUTING -d %s/32 ! -i lo -p %s -m %s --dport %d -j DROP", ip.String(), parts[1], parts[1], number)] = true
			}
		}
		for n := range c.NetworkSettings.Networks {
			if networks[n] != app {
				return fmt.Errorf("foreign container attachment %s", n)
			}
		}
	}
	return nil
}

// Docker 29 reports an object; older engines reported a string or omitted it.
func dockerIPTablesBackend(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name == "" || name == "iptables"
	}
	var backend struct{ Driver string }
	return json.Unmarshal(raw, &backend) == nil && backend.Driver == "iptables"
}
