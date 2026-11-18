package config

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// RateRule blocks or enforces a sliding per-client limit on a path.
type RateRule struct {
	Path     string  `yaml:"path"`
	Requests *int    `yaml:"requests,omitempty" json:",omitempty"`
	Window   *string `yaml:"window,omitempty" json:",omitempty"`
	Block    bool    `yaml:"block,omitempty" json:",omitempty"`
}

// MatchPath gives the canonical Caddy path matcher.
func (r RateRule) MatchPath() string {
	return strings.ToLower(r.Path)
}

func (r *RateRule) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: rate_limit rule must be a mapping", value.Line)
	}
	if err := checkMappingKeys(value, "config.RateRule", "path", "requests", "window", "block"); err != nil {
		return err
	}
	type rule RateRule
	return value.Decode((*rule)(r))
}

// Redirect sends requests matching Path to a fixed location.
type Redirect struct {
	Path      string `yaml:"path"`
	To        string `yaml:"to"`
	Permanent bool   `yaml:"permanent,omitempty" json:",omitempty"`
}

func (r *Redirect) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: redirect rule must be a mapping", value.Line)
	}
	if err := checkMappingKeys(value, "config.Redirect", "path", "to", "permanent"); err != nil {
		return err
	}
	type rule Redirect
	return value.Decode((*rule)(r))
}

// RedirectsBySpecificity returns the rules in evaluation order, independent
// of manifest order. An exact match wins over an equally long prefix.
func (h HTTP) RedirectsBySpecificity() []Redirect {
	rules := slices.Clone(h.Redirects)
	slices.SortFunc(rules, func(a, b Redirect) int {
		if order := cmp.Compare(len(strings.TrimSuffix(b.Path, "*")), len(strings.TrimSuffix(a.Path, "*"))); order != 0 {
			return order
		}
		if order := cmp.Compare(len(a.Path), len(b.Path)); order != 0 {
			return order
		}
		return cmp.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
	return rules
}

const (
	maxRateRules      = 16
	maxRedirects      = 32
	maxRateRequests   = 1000 // The plugin allocates a ring buffer per IP and rule.
	maxIngressPathLen = 1024
)

func (h HTTP) validateIngressRules() error {
	if len(h.RateLimit) > maxRateRules {
		return fmt.Errorf("rate_limit supports at most %d rules per service", maxRateRules)
	}
	if len(h.Redirects) > maxRedirects {
		return fmt.Errorf("redirects supports at most %d rules per service", maxRedirects)
	}
	seen := make(map[string]bool)
	for i, r := range h.RateLimit {
		if !validIngressPath(r.Path) {
			return fmt.Errorf("rate_limit[%d].path must be an exact path or end in /*", i)
		}
		p := r.MatchPath()
		if seen[p] {
			return fmt.Errorf("rate_limit[%d].path is a duplicate matcher", i)
		}
		seen[p] = true
		if r.Block {
			if r.Requests != nil || r.Window != nil {
				return fmt.Errorf("rate_limit[%d].block cannot be combined with requests or window", i)
			}
			continue
		}
		if r.Requests == nil || *r.Requests < 1 || *r.Requests > maxRateRequests {
			return fmt.Errorf("rate_limit[%d].requests must be between 1 and %d", i, maxRateRequests)
		}
		if r.Window == nil {
			return fmt.Errorf("rate_limit[%d].window is required", i)
		}
		if d, err := time.ParseDuration(*r.Window); err != nil || d < time.Second || d > time.Hour {
			return fmt.Errorf("rate_limit[%d].window must be a duration between 1s and 1h (for example 30s or 1m)", i)
		}
	}
	clear(seen)
	for i, r := range h.Redirects {
		if !validIngressPath(r.Path) {
			return fmt.Errorf("redirects[%d].path must be an exact path or end in /*", i)
		}
		p := strings.ToLower(r.Path)
		if seen[p] {
			return fmt.Errorf("redirects[%d].path is a duplicate matcher", i)
		}
		seen[p] = true
		if redirectTarget(r.To) == nil {
			return fmt.Errorf("redirects[%d].to must be a literal absolute path or HTTPS URL without credentials", i)
		}
	}
	return validateLocalRedirects(h.RedirectsBySpecificity())
}

// Matchers use unescaped URL path characters, without Caddy placeholders,
// glob syntax or alternative spellings that normalize to the same path.
var ingressPath = regexp.MustCompile(`^/[a-zA-Z0-9._~!$&'()+,;=:@/-]*$`)

func validIngressPath(pattern string) bool {
	p := strings.TrimSuffix(pattern, "*")
	return len(pattern) <= maxIngressPathLen && (p == pattern || strings.HasSuffix(p, "/")) &&
		ingressPath.MatchString(p) && !strings.Contains(p, "//") &&
		(path.Clean(p) == p || path.Clean(p)+"/" == p)
}

func redirectTarget(location string) *url.URL {
	if len(location) == 0 || len(location) > 2048 || strings.ContainsFunc(location, func(r rune) bool {
		return r < '!' || r > '~' || strings.ContainsRune("{}\"\\", r)
	}) {
		return nil
	}
	u, err := url.Parse(location)
	if err != nil {
		return nil
	}
	if _, err := url.PathUnescape(location); err != nil {
		return nil
	}
	if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		return u
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if u.Scheme != "https" || u.User != nil || strings.HasSuffix(u.Host, ":") || (!ValidDomain(host) && !validLabel(host) && net.ParseIP(host) == nil) {
		return nil
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil
		}
	}
	return u
}

// validateLocalRedirects follows fixed same-site destinations through the
// winning matcher for each path. External URLs are outside this check.
func validateLocalRedirects(rules []Redirect) error {
	next := make([]int, len(rules))
	for i, r := range rules {
		next[i] = -1
		if !strings.HasPrefix(r.To, "/") {
			continue
		}
		target := redirectTarget(r.To).Path
		clean := strings.ToLower(path.Clean(target))
		if clean != "/" && strings.HasSuffix(target, "/") {
			clean += "/"
		}
		for j, rule := range rules {
			pattern := strings.ToLower(rule.Path)
			prefix, wildcard := strings.CutSuffix(pattern, "*")
			if pattern == clean || (wildcard && strings.HasPrefix(clean, prefix)) {
				next[i] = j
				break
			}
		}
	}
	for start := range rules {
		steps := 0
		for i := start; i >= 0; i = next[i] {
			if steps == len(rules) {
				return fmt.Errorf("redirects: %q leads to a local redirect loop", rules[start].Path)
			}
			steps++
		}
	}
	return nil
}
