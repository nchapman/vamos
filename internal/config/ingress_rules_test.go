package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const ingressManifest = "app: demo\nservices:\n  web:\n    image: nginx:latest\n    http:\n      port: 8080\n"

func TestIngressRulesDecodeAndRoundTrip(t *testing.T) {
	a, err := Decode(strings.NewReader(ingressManifest + `      rate_limit:
        - path: "/*"
          requests: 120
          window: 1m
        - path: /login
          requests: 10
          window: 30s
        - path: /legacy/*
          block: true
      redirects:
        - path: /old/*
          to: /docs
        - path: /old/special
          to: https://docs.example.com/new?from=old#intro
          permanent: true
`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip App
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if err := roundTrip.Validate(); err != nil {
		t.Fatal(err)
	}
	h := roundTrip.Services["web"].HTTP
	if len(h.RateLimit) != 3 || h.RateLimit[0].MatchPath() != "/*" || h.RateLimit[1].MatchPath() != "/login" || *h.RateLimit[1].Requests != 10 || *h.RateLimit[1].Window != "30s" || !h.RateLimit[2].Block {
		t.Fatalf("unexpected rate rules after round trip: %+v", h.RateLimit)
	}
	rules := h.RedirectsBySpecificity()
	if len(rules) != 2 || rules[0].Path != "/old/special" || rules[0].To != "https://docs.example.com/new?from=old#intro" || !rules[0].Permanent || rules[1].Permanent || h.Redirects[0].Path != "/old/*" {
		t.Fatalf("unexpected redirect rules after round trip: %+v", rules)
	}
}

func TestIngressRulesRejectInvalidInput(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"unknown rate field", `rate_limit: [{path: /, requests: 2, window: 1m, key: IP}]`, "field key not found"},
		{"unknown redirect field", `redirects: [{path: /old, to: /new, status: 301}]`, "field status not found"},
		{"empty path", `rate_limit: [{path: '', requests: 2, window: 1m}]`, ".path"},
		{"missing path", `rate_limit: [{requests: 2, window: 1m}]`, ".path"},
		{"missing block path", `rate_limit: [{block: true}]`, ".path"},
		{"block with requests", `rate_limit: [{path: /legacy/*, requests: 0, block: true}]`, "cannot be combined"},
		{"block with window", `rate_limit: [{path: /legacy/*, window: '', block: true}]`, "cannot be combined"},
		{"zero requests", `rate_limit: [{path: /, requests: 0, window: 1m}]`, ".requests"},
		{"large requests", `rate_limit: [{path: /, requests: 1001, window: 1m}]`, ".requests"},
		{"invalid window", `rate_limit: [{path: /, requests: 2, window: 0s}]`, ".window"},
		{"too long window", `rate_limit: [{path: /, requests: 2, window: 2h}]`, ".window"},
		{"duplicate rate paths", `rate_limit: [{path: /Login, requests: 2, window: 1m}, {path: /login, requests: 3, window: 1m}]`, "duplicate"},
		{"duplicate global rules", `rate_limit: [{path: '/*', requests: 2, window: 1m}, {path: '/*', requests: 3, window: 1m}]`, "duplicate"},
		{"missing redirect path", `redirects: [{to: /new}]`, ".path"},
		{"embedded wildcard", `redirects: [{path: /old*pages, to: /new}]`, ".path"},
		{"query matcher", `redirects: [{path: '/old?page=2', to: /new}]`, ".path"},
		{"escaped matcher", `redirects: [{path: '/old%2Fpages', to: /new}]`, ".path"},
		{"unclean matcher", `redirects: [{path: '/old/../pages', to: /new}]`, ".path"},
		{"protocol relative", `redirects: [{path: /old, to: '//outside.example/a'}]`, ".to"},
		{"placeholder", `redirects: [{path: /old, to: '/next?token={uri}'}]`, ".to"},
		{"credentials", `redirects: [{path: /old, to: 'https://user@outside.example/a'}]`, ".to"},
		{"line injection", `redirects: [{path: /old, to: "/new\nimport foo"}]`, ".to"},
		{"duplicate redirect paths", `redirects: [{path: /Old, to: /new}, {path: /old, to: /other}]`, "duplicate"},
		{"redirect to same path", `redirects: [{path: /old, to: '/%4Fld?new=1'}]`, "loop"},
		{"redirect cycle", `redirects: [{path: /old/*, to: /new}, {path: /new, to: /old/page}]`, "loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(ingressManifest + "      " + tc.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}
