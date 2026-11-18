package deploy

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestLegacyDeployRejectsIngressRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*config.HTTP)
	}{
		{"rate limit", func(h *config.HTTP) {
			h.RateLimit = []config.RateRule{{Path: "/*", Requests: new(2), Window: new("1m")}}
		}},
		{"redirect", func(h *config.HTTP) { h.Redirects = []config.Redirect{{Path: "/old", To: "/new"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := app()
			tc.edit(a.Services["web"].HTTP)
			if err := validateLegacyApp(a); err == nil || !strings.Contains(err.Error(), "current client") {
				t.Fatalf("legacy deploy did not reject %s: %v", tc.name, err)
			}
		})
	}
}

func TestCaddyRulesRenderCandidateOnlyInHandlerOrder(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	api := j.ServiceStates["api"].Candidate
	cfg := *api.Config
	h := *cfg.HTTP
	h.Domains = []string{"api.other.example.com"}
	h.RateLimit = []config.RateRule{
		{Path: "/LOGIN", Requests: new(10), Window: new("60s")},
		{Path: "/*", Requests: new(100), Window: new("1m")},
		{Path: "/legacy/*", Block: true},
	}
	h.Redirects = []config.Redirect{{Path: "/old/*", To: "https://docs.example.com/?from=old#intro"}, {Path: "/old/pricing", To: "/pricing", Permanent: true}}
	cfg.HTTP = &h
	api.Config = &cfg

	current, err := m.routes(j, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(current, "rate_limit") || strings.Contains(current, "redir ") || strings.Contains(current, "api.other.example.com") {
		t.Fatal("pending rules leaked into the current routes")
	}
	candidate, err := m.routes(j, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.other.example.com", "notes-api.example.com"} {
		_, rest, ok := strings.Cut(candidate, "\n"+host+" {\n\troute {\n")
		site, _, closed := strings.Cut(rest, "\n}\n")
		if !ok || !closed {
			t.Fatalf("missing explicit route for %s: %s", host, candidate)
		}
		for _, directive := range []string{"coraza_waf {", "respond /legacy/* 403", "rate_limit {", "path /*", "path /login", "events 10\n\t\t\t\twindow 1m0s", `redir /old/pricing "/pricing" 301`, `redir /old/* "https://docs.example.com/?from=old#intro" 302`, "\t\tcache\n", "encode zstd gzip", "reverse_proxy 127.0.0.1:12002"} {
			_, site, ok = strings.Cut(site, directive)
			if !ok {
				t.Fatalf("%s: missing or out-of-order directive %s in %s", host, directive, candidate)
			}
		}
	}
	if !strings.Contains(candidate, "\nnotes.example.com {\n\tcoraza_waf {") {
		t.Fatal("untouched host's handler layout changed")
	}

	zones := regexp.MustCompile(`zone (vamos_\w+)`).FindAllStringSubmatch(candidate, -1)
	if len(zones) != 4 {
		t.Fatalf("expected four zones, got %v", zones)
	}
	seen := make(map[string]bool)
	for _, zone := range zones {
		if seen[zone[1]] {
			t.Fatalf("zone shared by distinct rules: %s", zone[1])
		}
		seen[zone[1]] = true
	}
	slices.Reverse(h.RateLimit)
	slices.Reverse(h.Redirects)
	h.RateLimit[2].Window = new("1m")
	reordered, err := m.routes(j, true)
	if err != nil || candidate != reordered {
		t.Fatalf("equivalent config changed rendered routes: %v\n%s", err, reordered)
	}
}
