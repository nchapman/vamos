package deploy

import (
	"errors"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/config"
)

func domainConfig(a *config.App) bool {
	if a == nil {
		return false
	}
	for _, s := range a.Services {
		if s.HTTP != nil && len(s.HTTP.Domains) > 0 {
			return true
		}
	}
	return false
}

// Generated names always remain routes; aliases are canonical DNS names rather
// than Caddyfile fragments. Dedupe an alias equal to its own generated hostname.
func serviceHostnames(app, service, base string, cfg *config.Service) ([]string, error) {
	hosts := map[string]bool{strings.TrimPrefix(ServiceURL(app, service, base), "https://"): true}
	if cfg != nil && cfg.HTTP != nil {
		for _, name := range cfg.HTTP.Domains {
			hosts[strings.ToLower(name)] = true
		}
	}
	out := make([]string, 0, len(hosts))
	for name := range hosts {
		if !config.ValidDomain(name) {
			return nil, errors.New("invalid public hostname")
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Keep both effective and desired aliases unavailable during single-writer
// replacement. Promotion alone releases removed aliases and admits new ones.
func unavailableHostnames(j *journal, name, base string, slot *serviceState) ([]string, error) {
	hosts := map[string]bool{}
	configs := []*config.Service{}
	if slot.Current != nil {
		configs = append(configs, slot.Current.Config)
	}
	if slot.Candidate != nil {
		configs = append(configs, slot.Candidate.Config)
	}
	if len(configs) == 0 {
		cfg := j.Config.Services[name]
		configs = append(configs, &cfg)
	}
	for _, cfg := range configs {
		names, err := serviceHostnames(j.App, name, base, cfg)
		if err != nil {
			return nil, err
		}
		for _, host := range names {
			hosts[host] = true
		}
	}
	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}
