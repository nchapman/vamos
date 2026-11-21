package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/distribution/reference"
)

var (
	labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	envPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks local configuration only. Server-wide hostname ownership,
// secret availability, image validity, and storage state require deploy preflight.
func (a App) Validate() error {
	var issues []error
	add := func(format string, args ...any) {
		issues = append(issues, fmt.Errorf(format, args...))
	}
	if !validLabel(a.Name) {
		add("app must be a lowercase DNS label (1–63 characters)")
	}
	if len(a.Services) == 0 {
		add("services must contain at least one service")
	}

	totalReplicas := 0
	totalSecrets := 0
	domains := make(map[string]string)
	volumes := make(map[string]string)
	type portClaim struct {
		mapping PortMapping
		service string
	}
	var ports []portClaim
	for _, name := range sortedKeys(a.Services) {
		s := a.Services[name]
		prefix := "services." + name
		count := s.ReplicaCount()
		if count < 1 {
			add("%s.replicas must be positive", prefix)
		} else if count > MaxAppReplicas-totalReplicas {
			totalReplicas = MaxAppReplicas + 1
		} else {
			totalReplicas += count
		}
		if s.Volume != nil && count > 1 {
			add("%s: volume-backed services cannot have multiple replicas", prefix)
		}
		if len(s.Ports) != 0 && count != 1 {
			add("%s: services with ports require one replica", prefix)
		}
		bindings := 0
		for _, value := range s.Ports {
			mapping, err := ParsePort(value)
			if err != nil {
				add("%s: %v", prefix, err)
				continue
			}
			bindings += mapping.Count
			if bindings > MaxPortBindings {
				add("%s.ports: at most %d published bindings per service are supported (ranges count individually)", prefix, MaxPortBindings)
				break
			}
			if mapping.Reserved() {
				add("%s.ports: %q overlaps a reserved Vamos ingress/admin port", prefix, value)
			}
			for _, prior := range ports {
				if mapping.Overlaps(prior.mapping) {
					add("%s.ports: %q overlaps a binding in services.%s", prefix, value, prior.service)
				}
			}
			ports = append(ports, portClaim{mapping, name})
		}
		if !validLabel(name) {
			add("%s: service name must be a lowercase DNS label (1–63 characters)", prefix)
		}
		if (s.Build != nil) == (s.Image != "") {
			add("%s: specify exactly one of build or image", prefix)
		}
		if s.Build != nil {
			if s.Build.Context == "" || path.IsAbs(s.Build.Context) || strings.ContainsAny(s.Build.Context, "\x00\\") {
				add("%s.build.context must be a nonempty relative directory", prefix)
			}
			dockerfile := s.Build.Dockerfile
			clean := path.Clean(dockerfile)
			if dockerfile == "" || path.IsAbs(dockerfile) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(dockerfile, "\x00\\") {
				add("%s.build.dockerfile must be a file path within the build context", prefix)
			}
		}
		if strings.ContainsAny(s.Image, " \t\r\n\x00") {
			add("%s.image must not contain whitespace or NUL", prefix)
		} else if s.Image != "" && !ValidImageReference(s.Image) {
			add("%s.image: invalid image reference", prefix)
		}
		if s.Update != UpdateAuto && s.Update != UpdateManual {
			add("%s.update must be auto or manual", prefix)
		}
		if s.Command != nil && (len(s.Command) == 0 || s.Command[0] == "") {
			add("%s.command must contain a nonempty executable", prefix)
		}
		for _, arg := range s.Command {
			if strings.ContainsRune(arg, '\x00') {
				add("%s.command must not contain NUL", prefix)
				break
			}
		}
		for _, key := range sortedKeys(s.Env) {
			if !envPattern.MatchString(key) {
				add("%s.env: invalid environment variable name %q", prefix, key)
			}
			if strings.ContainsRune(s.Env[key], '\x00') {
				add("%s.env.%s must not contain NUL", prefix, key)
			}
		}
		totalSecrets += len(s.Secrets)
		for _, key := range sortedKeys(s.Secrets) {
			if !envPattern.MatchString(key) {
				add("%s.secrets: invalid environment variable name %q", prefix, key)
			}
			if !validLabel(s.Secrets[key]) {
				add("%s.secrets.%s: secret name %q must reference a lowercase DNS-label secret name (e.g. %q)", prefix, key, s.Secrets[key], suggestSecretName(s.Secrets[key]))
			}
			if _, exists := s.Env[key]; exists {
				add("%s: %s cannot appear in both env and secrets", prefix, key)
			}
		}
		if s.HTTP != nil {
			if err := s.HTTP.validateIngressRules(); err != nil {
				add("%s.http.%w", prefix, err)
			}
			if name != "web" && len(a.Name+"-"+name) > 63 {
				add("%s: generated hostname label exceeds 63 bytes", prefix)
			}
			if s.HTTP.Port < 1 || s.HTTP.Port > 65535 {
				add("%s.http.port must be between 1 and 65535", prefix)
			}
			health := s.HTTP.Health
			u, err := url.ParseRequestURI(health)
			if err != nil || !strings.HasPrefix(health, "/") || strings.HasPrefix(health, "//") || strings.ContainsAny(health, "#\r\n") || (u != nil && u.IsAbs()) {
				add("%s.http.health must be a local HTTP path starting with /", prefix)
			}
			for _, domain := range s.HTTP.Domains {
				normalized := strings.ToLower(domain)
				if !ValidDomain(normalized) {
					add("%s.http.domains: %q must be a DNS hostname, without a scheme, port, or wildcard", prefix, domain)
				}
				if owner, exists := domains[normalized]; exists {
					add("%s.http.domains: %q is already assigned to %s", prefix, domain, owner)
				}
				domains[normalized] = prefix
			}
		}
		if err := s.Resources.Validate(); err != nil {
			add("%s.resources: %v", prefix, err)
		}
		if volume := s.Volume; volume != nil {
			if !validLabel(volume.Name) {
				add("%s.volume: volume name must be a lowercase DNS label", prefix)
			}
			if !path.IsAbs(volume.Mount) || path.Clean(volume.Mount) != volume.Mount || volume.Mount == "/" || strings.ContainsRune(volume.Mount, '\x00') {
				add("%s.volume: mount must be a clean absolute container directory other than /", prefix)
			}
			if owner, exists := volumes[volume.Name]; exists {
				add("%s.volume: volume %q is already mounted by %s; shared volumes are not supported", prefix, volume.Name, owner)
			}
			volumes[volume.Name] = prefix
		}
	}
	if r := a.Release; r != nil {
		if _, ok := a.Services[r.Service]; !ok || !validLabel(r.Service) {
			add("release.service must name a declared service")
		}
		if len(r.Command) == 0 || len(r.Command) > 256 || r.Command[0] == "" {
			add("release.command must contain a nonempty executable and at most 256 arguments")
		}
		total := 0
		for _, arg := range r.Command {
			total += len(arg)
			if len(arg) > 8192 || strings.ContainsRune(arg, '\x00') {
				add("release.command arguments must be at most 8192 bytes and contain no NUL")
			}
		}
		if total > 32<<10 {
			add("release.command must be at most 32768 bytes total")
		}
	}
	if totalReplicas > MaxAppReplicas {
		add("at most %d replicas per app are supported", MaxAppReplicas)
	}
	if totalSecrets > MaxSecretMappings {
		add("at most %d secret environment mappings across the app are supported", MaxSecretMappings)
	}
	if len(domains) > 32 {
		add("at most 32 custom domains per app are supported")
	}
	return errors.Join(issues...)
}

// ValidImageReference reports whether ref is a valid normalized Docker image reference.
func ValidImageReference(ref string) bool {
	if len(ref) == 0 || len(ref) > 1024 || strings.HasPrefix(ref, "-") || strings.Contains(ref, "://") || strings.ContainsFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	_, err := reference.ParseNormalizedNamed(ref)
	return err == nil
}

func suggestSecretName(raw string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "my-secret"
	}
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

func validLabel(value string) bool {
	return len(value) >= 1 && len(value) <= 63 && labelPattern.MatchString(value)
}

// ValidDomain accepts a lowercase DNS hostname, not a URL, wildcard or IP address.
func ValidDomain(value string) bool {
	if len(value) > 253 || !strings.Contains(value, ".") || net.ParseIP(value) != nil {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
