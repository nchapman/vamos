package secrets

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// Redact replaces exact values (and common encoded forms for secrets >= 8 bytes)
// from every retained version, longest match first.
// Replacements are not rescanned, including when a secret is a single character.
func (s *State) Redact(text string) string {
	if s == nil {
		return text
	}
	seen := map[string]bool{}
	values := []string{}
	add := func(candidate string) {
		if candidate != "" && !seen[candidate] {
			seen[candidate] = true
			values = append(values, candidate)
		}
	}
	for _, v := range s.data.Versions {
		add(v.Value)
		if len(v.Value) >= 8 {
			add(url.QueryEscape(v.Value))
			add(url.PathEscape(v.Value))
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				add(enc.EncodeToString([]byte(v.Value)))
			}
			for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
				for shift := 1; shift <= 2; shift++ {
					fullBlocks := (len(v.Value) - (3 - shift)) / 3
					if fullBlocks >= 2 {
						padded := strings.Repeat("\x00", shift) + v.Value
						encoded := enc.EncodeToString([]byte(padded))
						add(encoded[4 : 4+fullBlocks*4])
					}
				}
			}
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
