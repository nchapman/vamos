package secrets

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestRedactAllVersionsSinglePass(t *testing.T) {
	s := &State{data: diskState{Versions: map[string]diskValue{"old": {Value: "abc"}, "new": {Value: "abcdef"}, "short": {Value: "x"}, "replacement": {Value: "REDACTED"}}}}
	if got := s.Redact("abcdef abc x"); got != "[REDACTED] [REDACTED] [REDACTED]" {
		t.Fatal(got)
	}
	var absent *State
	if absent.Redact("ok") != "ok" {
		t.Fatal("nil")
	}
}

func TestRedactEncodedAndCompositeBase64(t *testing.T) {
	secret := "super-secret-token-value/123+456"
	s := &State{data: diskState{Versions: map[string]diskValue{"v1": {Value: secret}}}}
	if got := s.Redact("token=" + url.QueryEscape(secret)); got != "token=[REDACTED]" {
		t.Fatalf("query-escaped secret not redacted: %s", got)
	}
	if got := s.Redact("b64=" + base64.StdEncoding.EncodeToString([]byte(secret))); got != "b64=[REDACTED]" {
		t.Fatalf("base64 secret not redacted: %s", got)
	}
	for _, prefix := range []string{"u:", "us:", "usr:"} {
		basic := base64.StdEncoding.EncodeToString([]byte(prefix + secret))
		redacted := s.Redact("Authorization: Basic " + basic)
		if !strings.Contains(redacted, "[REDACTED]") {
			t.Fatalf("composite basic auth with prefix %q was not redacted: %s", prefix, redacted)
		}
	}
}
