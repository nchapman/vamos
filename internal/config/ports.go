package config

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxPortBindings limits Docker's per-port NAT/proxy and inspection work per service.
const MaxPortBindings = 4096

// PortMapping is a Docker-style, IPv4 public binding. Ranges map one-to-one.
// Keep ranges compact for validation; Docker expands them at container creation.
type PortMapping struct {
	Host, Container, Count int
	Protocol               string
}

func ParsePort(value string) (PortMapping, error) {
	bad := fmt.Errorf("ports: %q must be HOST:CONTAINER[/tcp|udp] with equal-sized ranges and ports 1..65535 (no host IP)", value)
	base, protocol, hasProtocol := strings.Cut(value, "/")
	if !hasProtocol {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return PortMapping{}, bad
	}
	host, container, ok := strings.Cut(base, ":")
	if !ok {
		return PortMapping{}, bad
	}
	parse := func(s string) (int, int, bool) {
		first, last, ranged := strings.Cut(s, "-")
		number := func(v string) (int, bool) {
			if v == "" {
				return 0, false
			}
			for _, c := range v {
				if c < '0' || c > '9' {
					return 0, false
				}
			}
			n, err := strconv.Atoi(v)
			return n, err == nil && n >= 1 && n <= 65535
		}
		start, ok := number(first)
		if !ok {
			return 0, 0, false
		}
		end := start
		if ranged {
			end, ok = number(last)
		}
		return start, end, ok && end >= start
	}
	h, he, ok := parse(host)
	if !ok {
		return PortMapping{}, bad
	}
	c, ce, ok := parse(container)
	if !ok || he-h != ce-c {
		return PortMapping{}, bad
	}
	return PortMapping{Host: h, Container: c, Count: he - h + 1, Protocol: protocol}, nil
}

func (p PortMapping) Overlaps(q PortMapping) bool {
	return p.Protocol == q.Protocol && p.Host < q.Host+q.Count && q.Host < p.Host+p.Count
}

// Ingress uses TCP 80/443, HTTP/3 uses UDP 443, and the Caddy admin API
// uses loopback TCP 2019. A wildcard IPv4 publish would overlap loopback too.
func (p PortMapping) Reserved() bool {
	for _, q := range []PortMapping{{Host: 80, Count: 1, Protocol: "tcp"}, {Host: 443, Count: 1, Protocol: "tcp"}, {Host: 443, Count: 1, Protocol: "udp"}, {Host: 2019, Count: 1, Protocol: "tcp"}} {
		if p.Overlaps(q) {
			return true
		}
	}
	return false
}

// StopFirst services cannot run old and new containers concurrently.
func (s Service) StopFirst() bool { return s.Volume != nil || len(s.Ports) != 0 }
