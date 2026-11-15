package deploy

import (
	"context"
	"crypto/tls"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nchapman/vamos/internal/config"
)

// DomainStatus describes aliases only; certificate readiness is a local observation,
// not a guarantee that the endpoint is reachable from the Internet.
type DomainStatus struct {
	Hostname    string `json:"hostname"`
	DNS         string `json:"dns"`
	Certificate string `json:"certificate"`
	HTTPS       string `json:"https"`
	Route       string `json:"route"`
}

func domainAliases(s *config.Service) []string {
	if s == nil || s.HTTP == nil {
		return nil
	}
	return s.HTTP.Domains
}

func (m *Manager) enrichDomainResponse(ctx context.Context, r Request, resp *Response) {
	if resp.Status == nil || len(resp.Status.Services) == 0 {
		return
	}
	j, err := m.load(r.App)
	if err != nil {
		return
	}
	names := make([]string, 0, len(resp.Status.Services))
	for name := range resp.Status.Services {
		if r.Service == "" || r.Service == name {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type task struct {
		service string
		index   int
	}
	var tasks []task
	for _, name := range names {
		ss := resp.Status.Services[name]
		aliases := map[string]bool{}
		slot := j.ServiceStates[name]
		if slot != nil && slot.Current != nil {
			for _, h := range domainAliases(slot.Current.Config) {
				aliases[strings.ToLower(h)] = true
			}
		}
		// Only captured desired state is authoritative. A scoped request may
		// contain unrelated local edits that were deliberately not deployed.
		desired := j.Config
		if desired != nil {
			cfg := desired.Services[name]
			for _, h := range domainAliases(&cfg) {
				h = strings.ToLower(h)
				if _, ok := aliases[h]; !ok {
					aliases[h] = false
				}
			}
		}
		hosts := make([]string, 0, len(aliases))
		for h := range aliases {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		ss.Domains = nil
		for _, h := range hosts {
			d := DomainStatus{Hostname: h, Route: "pending", DNS: "unknown", Certificate: "unknown", HTTPS: "unknown"}
			if aliases[h] || (slot != nil && slot.Unavailable) {
				d.Route = "configured"
				if slot.Unavailable || slot.CurrentRetired || slot.Phase == "retiring" || j.Phase == "destroying" || (j.Maintenance != nil && j.Maintenance.Action == "service-remove" && j.Maintenance.Service == name) {
					d.Route = "unavailable"
				}
			}
			// A write/reload may have succeeded without its acknowledgement.
			// Until reconciliation, neither old nor new routes are authoritative.
			if j.NewCaddy != "" {
				d.Route = "pending"
			}
			ss.Domains = append(ss.Domains, d)
			if r.Action == "status" && !resp.Status.Active && d.Route == "configured" {
				tasks = append(tasks, task{name, len(ss.Domains) - 1})
			}
		}
		resp.Status.Services[name] = ss
	}
	if len(tasks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	probe := m.DomainProbe
	if probe == nil {
		probe = probeDomain
	}
	var mu sync.Mutex
	jobs := make(chan task)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				mu.Lock()
				d := resp.Status.Services[t.service].Domains[t.index]
				mu.Unlock()
				p := probe(ctx, d.Hostname)
				switch p.DNS {
				case "matches-host", "pending", "unverified":
					d.DNS = p.DNS
				}
				switch p.Certificate {
				case "ready", "pending":
					d.Certificate = p.Certificate
				}
				d.HTTPS = "pending"
				if d.DNS == "matches-host" && d.Certificate == "ready" {
					d.HTTPS = "ready"
				}
				mu.Lock()
				resp.Status.Services[t.service].Domains[t.index] = d
				mu.Unlock()
			}
		}()
	}
	for _, t := range tasks {
		select {
		case jobs <- t:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
}

func classifyDomainDNS(resolved, local []net.IP) string {
	if len(resolved) == 0 {
		return "pending"
	}
	for _, ip := range resolved {
		match := false
		if ip.IsGlobalUnicast() && !ip.IsPrivate() {
			for _, host := range local {
				if ip.Equal(host) {
					match = true
					break
				}
			}
		}
		if !match {
			return "unverified"
		}
	}
	return "matches-host"
}

func probeDomain(ctx context.Context, hostname string) DomainStatus {
	d := DomainStatus{Hostname: hostname, DNS: "pending", Certificate: "pending"}
	// DNS and TLS are independent observations, sharing one finite deadline.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
		if err != nil {
			return
		}
		var local []net.IP
		addresses, err := net.InterfaceAddrs()
		if err == nil {
			for _, addr := range addresses {
				ip, _, err := net.ParseCIDR(addr.String())
				if err == nil {
					local = append(local, ip)
				}
			}
		}
		d.DNS = classifyDomainDNS(ips, local)
	}()
	dialer := tls.Dialer{Config: &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:443")
	if err == nil {
		d.Certificate = "ready"
		conn.Close()
	}
	wg.Wait()
	return d
}
