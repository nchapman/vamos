package secrets

import "testing"

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
