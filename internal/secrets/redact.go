package secrets

import (
	"sort"
	"strings"
)

// Redact replaces exact values from every retained version, longest match first.
// It cannot recognize encoded values, unknown secrets, or other apps' secrets.
// Replacements are not rescanned, including when a secret is a single character.
func (s *State) Redact(text string) string {
	if s == nil {
		return text
	}
	values := []string{}
	for _, v := range s.data.Versions {
		if v.Value != "" {
			values = append(values, v.Value)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := []string{}
	for _, v := range values {
		pairs = append(pairs, v, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
