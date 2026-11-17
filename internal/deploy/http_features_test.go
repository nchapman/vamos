package deploy

import (
	"strings"
	"testing"
)

func TestHTTPFeaturesAreIndependentPerRoute(t *testing.T) {
	for _, feature := range []string{"waf", "cache", "compression"} {
		t.Run(feature, func(t *testing.T) {
			m, _ := setup(t)
			j := routeTestJournal("notes")
			disabled := false
			h := *j.ServiceStates["api"].Candidate.Config.HTTP
			switch feature {
			case "waf":
				h.WAF = &disabled
			case "cache":
				h.Cache = &disabled
			case "compression":
				h.Compression = &disabled
			}
			cfg := *j.ServiceStates["api"].Candidate.Config
			cfg.HTTP = &h
			j.ServiceStates["api"].Candidate.Config = &cfg
			routes, err := m.routes(j, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, setting := range []string{"default_cache_control no-store", "max_cacheable_body_bytes 10485760", "headers Authorization", "hide"} {
				if !strings.Contains(routes, setting) {
					t.Fatalf("missing global cache setting %q in rendered Caddyfile: %s", setting, routes)
				}
			}
			api := strings.Split(strings.Split(routes, "\nnotes-api.example.com {\n")[1], "\n}\n")[0]
			web := strings.Split(strings.Split(routes, "\nnotes.example.com {\n")[1], "\n}\n")[0]
			for name, directive := range map[string]string{"waf": "\tcoraza_waf {", "cache": "\tcache\n", "compression": "\tencode zstd gzip\n"} {
				if strings.Contains(api, directive) != (name != feature) {
					t.Fatalf("unexpected %s in API route: %s", name, api)
				}
				if !strings.Contains(web, directive) {
					t.Fatalf("missing %s in unaffected web route: %s", name, web)
				}
			}
		})
	}
}
