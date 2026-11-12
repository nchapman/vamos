package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nchapman/soar/internal/lifecycle"
)

// ServiceURL returns the generated public URL. Callers validate DNS lengths
// before accepting a deployment; private services must not expose this URL.
func ServiceURL(app, service, domain string) string {
	if service != "web" {
		app += "-" + service
	}
	return "https://" + app + "." + domain
}

func routeDomain(j *journal) (string, error) {
	if j == nil || !label.MatchString(j.App) || len(j.App) > 63 || !operation.MatchString(j.HostID) {
		return "", errors.New("invalid app journal identity")
	}
	prefix := "https://" + j.App + "."
	if !strings.HasPrefix(j.URL, prefix) {
		return "", errors.New("invalid app journal URL")
	}
	domain := strings.TrimPrefix(j.URL, prefix)
	if lifecycle.ValidateConfig(lifecycle.Config{Name: "host", Domain: domain}) != nil {
		return "", errors.New("invalid app journal domain")
	}
	return domain, nil
}

// Include the persisted own journal as well as the prospective journal: neither
// an unfinished request nor a superseding request can release effective claims.
func (m *Manager) routeJournals(j *journal) ([]*journal, string, error) {
	domain, e := routeDomain(j)
	if e != nil {
		return nil, "", e
	}
	appsDir := m.path("/var/lib/soar/apps")
	if e := safe(appsDir); e != nil {
		return nil, "", e
	}
	entries, e := os.ReadDir(appsDir)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, "", e
	}
	all := []*journal{}
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, "", errors.New("unexpected app journal entry")
		}
		other, e := m.load(entry.Name())
		if entry.Name() == j.App && errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, "", e
		}
		if other.HostID != j.HostID {
			return nil, "", errors.New("foreign app journal")
		}
		if other.Phase == "destroyed" {
			continue
		}
		d, e := routeDomain(other)
		if e != nil {
			return nil, "", e
		}
		if d != domain {
			return nil, "", errors.New("foreign app journal domain")
		}
		all = append(all, other)
	}
	all = append(all, j)
	return all, domain, nil
}

type hostnameOwner struct{ app, service string }

func claimHostname(claims map[string]hostnameOwner, app, service, domain string) error {
	if !label.MatchString(service) || len(service) > 63 {
		return errors.New("invalid service hostname claim")
	}
	hostname := strings.TrimPrefix(ServiceURL(app, service, domain), "https://")
	owner := hostnameOwner{app, service}
	if prev, ok := claims[hostname]; ok && prev != owner {
		return fmt.Errorf("public hostname %s is claimed by both %s/%s and %s/%s", hostname, prev.app, prev.service, app, service)
	}
	claims[hostname] = owner
	return nil
}

func reserveJournalHostnames(all []*journal, domain string) error {
	claims := map[string]hostnameOwner{}
	for _, j := range all {
		if j.Phase == "destroyed" {
			continue
		}
		if j.Config == nil || j.Config.Name != j.App {
			return errors.New("invalid app hostname claims")
		}
		for name, cfg := range j.Config.Services {
			if cfg.HTTP != nil {
				if e := claimHostname(claims, j.App, name, domain); e != nil {
					return e
				}
			}
		}
		if j.Version != 2 {
			if j.Current != nil || j.Candidate != nil {
				if e := claimHostname(claims, j.App, "web", domain); e != nil {
					return e
				}
			}
			continue
		}
		for name, slot := range j.ServiceStates {
			if slot == nil {
				return errors.New("invalid service slot")
			}
			for _, r := range []*release{slot.Current, slot.Candidate, slot.Retiring} {
				if r == nil {
					continue
				}
				if r.Config == nil {
					return errors.New("missing effective service config")
				}
				if r.Config.HTTP != nil {
					if e := claimHostname(claims, j.App, name, domain); e != nil {
						return e
					}
				}
			}
		}
	}
	return nil
}

// reserveHostnames is a read-only global reservation check, including pending
// desired configs and the effective versions of partially updated apps.
func (m *Manager) reserveHostnames(j *journal) error {
	all, domain, e := m.routeJournals(j)
	if e != nil {
		return e
	}
	return reserveJournalHostnames(all, domain)
}

func (m *Manager) routes(j *journal, candidate bool) (string, error) {
	all, domain, e := m.routeJournals(j)
	if e != nil {
		return "", e
	}
	if e = reserveJournalHostnames(all, domain); e != nil {
		return "", e
	}
	routes := map[string]string{}
	add := func(app, service string, r *release) error {
		if r == nil {
			return nil
		}
		port, e := strconv.Atoi(r.Port)
		if e != nil || port < 1 || port > 65535 || strconv.Itoa(port) != r.Port {
			return errors.New("invalid public route port")
		}
		host := strings.TrimPrefix(ServiceURL(app, service, domain), "https://")
		if _, exists := routes[host]; exists {
			return errors.New("duplicate public route")
		}
		routes[host] = r.Port
		return nil
	}
	// Render the caller's own in-memory state, even before its first save. The
	// persisted own copy above is used only for identity/claim validation.
	for i, other := range all {
		if other.App == j.App && i != len(all)-1 {
			continue
		}
		if other.Phase == "destroyed" {
			continue
		}
		if other.App != j.App && other.NewCaddy != "" {
			return "", errors.New("another app has pending promotion; resume it first")
		}
		if other.Version != 2 {
			r := other.Current
			if other.App == j.App && candidate {
				r = j.Candidate
			}
			if e = add(other.App, "web", r); e != nil {
				return "", e
			}
			continue
		}
		for name, slot := range other.ServiceStates {
			r := slot.Current
			if other.App == j.App && candidate && name == j.ActiveService {
				r = slot.Candidate
			}
			if r == nil {
				continue
			}
			if r.Config == nil {
				return "", errors.New("missing effective service config")
			}
			if r.Config.HTTP == nil {
				continue
			}
			if e = add(other.App, name, r); e != nil {
				return "", e
			}
		}
	}
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := baseCaddy
	for _, k := range keys {
		out += fmt.Sprintf("\n%s {\n\treverse_proxy 127.0.0.1:%s\n}\n", k, routes[k])
	}
	return out, nil
}

// verifyRoutes checks the stable effective routes, not an uncommitted intent.
// It never repairs disk/runtime drift or clears a promotion intent.
func (m *Manager) verifyRoutes(c context.Context, j *journal) error {
	expected, e := m.routes(j, false)
	if e != nil {
		return e
	}
	p := m.path("/etc/caddy/Caddyfile")
	if e = safe(p); e != nil {
		return e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if string(b) != expected {
		return errors.New("Caddyfile manual drift")
	}
	adapted, e := m.adapt(c, expected)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	if !sameJSON(live, adapted) {
		return errors.New("Caddy runtime drift")
	}
	return nil
}

func (m *Manager) routeCandidateHealthy(c context.Context, j *journal) error {
	if j.Version != 2 {
		return m.healthy(c, j, j.Candidate)
	}
	if e := validateRouteCandidate(j); e != nil {
		return e
	}
	return m.multiHealthy(c, j, j.ServiceStates[j.ActiveService].Candidate)
}

func validateRouteCandidate(j *journal) error {
	if j.Version != 2 {
		return nil
	}
	slot := j.ServiceStates[j.ActiveService]
	if j.Phase != "updating" || slot == nil || slot.Phase != "promoting" || slot.Candidate == nil || slot.Candidate.Config == nil || slot.Candidate.Config.HTTP == nil {
		return errors.New("invalid service promotion")
	}
	return nil
}
