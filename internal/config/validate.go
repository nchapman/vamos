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

	domains := make(map[string]string)
	volumes := make(map[string]string)
	for _, name := range sortedKeys(a.Services) {
		s := a.Services[name]
		prefix := "services." + name
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
		for _, key := range sortedKeys(s.Secrets) {
			if !envPattern.MatchString(key) {
				add("%s.secrets: invalid environment variable name %q", prefix, key)
			}
			if !validLabel(s.Secrets[key]) {
				add("%s.secrets.%s must reference a lowercase DNS-label secret name", prefix, key)
			}
			if _, exists := s.Env[key]; exists {
				add("%s: %s cannot appear in both env and secrets", prefix, key)
			}
		}
		if s.HTTP != nil {
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
				if !validDomain(normalized) {
					add("%s.http.domains: %q must be a DNS hostname, without a scheme, port, or wildcard", prefix, domain)
				}
				if owner, exists := domains[normalized]; exists {
					add("%s.http.domains: %q is already assigned to %s", prefix, domain, owner)
				}
				domains[normalized] = prefix
			}
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
	return errors.Join(issues...)
}

func validLabel(value string) bool {
	return len(value) >= 1 && len(value) <= 63 && labelPattern.MatchString(value)
}

func validDomain(value string) bool {
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
