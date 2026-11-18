package deploy

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/config"
)

const corazaBlock = "\tcoraza_waf {\n\t\tload_owasp_crs\n\t\tdirectives `\n\t\t\tInclude @coraza.conf-recommended\n\t\t\tInclude @crs-setup.conf.example\n\t\t\tInclude @owasp_crs/*.conf\n\t\t\tSecRuleEngine On\n\t\t`\n\t}\n"

// writeCaddySite renders a site from validated service configuration.
func writeCaddySite(out *strings.Builder, host, upstreams string, h config.HTTP) {
	var handlers strings.Builder
	if h.WAFEnabled() {
		handlers.WriteString(corazaBlock)
	}
	writeRateRules(&handlers, host, h.RateLimit)
	for _, r := range h.RedirectsBySpecificity() {
		code := 302
		if r.Permanent {
			code = 301
		}
		fmt.Fprintf(&handlers, "\tredir %s %q %d\n", strings.ToLower(r.Path), r.To, code)
	}
	if h.CacheEnabled() {
		handlers.WriteString("\tcache\n")
	}
	if h.CompressionEnabled() {
		handlers.WriteString("\tencode zstd gzip\n")
	}
	fmt.Fprintf(&handlers, "\treverse_proxy %s", upstreams)
	if strings.Contains(upstreams, " ") {
		handlers.WriteString(" {\n\t\tlb_policy round_robin\n\t\tlb_try_duration 5s\n\t\tfail_duration 10s\n\t}")
	}
	fmt.Fprintf(out, "\n%s {\n", host)
	if len(h.RateLimit) > 0 || len(h.Redirects) > 0 {
		// Caddy's default directive order would run redir before the limiter.
		// Keep the old rendering for sites without rules to preserve drift checks.
		out.WriteString("\troute {\n")
		for line := range strings.SplitSeq(handlers.String(), "\n") {
			fmt.Fprintf(out, "\t%s\n", line)
		}
		out.WriteString("\t}\n")
	} else {
		out.WriteString(handlers.String())
		out.WriteByte('\n')
	}
	out.WriteString("}\n")
}

func writeRateRules(out *strings.Builder, host string, rules []config.RateRule) {
	rules = slices.Clone(rules)
	slices.SortFunc(rules, func(a, b config.RateRule) int { return strings.Compare(a.MatchPath(), b.MatchPath()) })
	hasLimits := false
	for _, rule := range rules {
		if rule.Block {
			fmt.Fprintf(out, "\trespond %s 403\n", rule.MatchPath())
		} else {
			hasLimits = true
		}
	}
	if !hasLimits {
		return
	}
	out.WriteString("\trate_limit {\n")
	for _, rule := range rules {
		if rule.Block {
			continue
		}
		match := rule.MatchPath()
		window, _ := time.ParseDuration(*rule.Window) // validated before rendering
		// Caddy shares zone state across reloads by name. Changing the policy
		// creates a new zone instead of mutating the old config's live buckets.
		zone := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d\x00%d", host, match, *rule.Requests, window))
		fmt.Fprintf(out, "\t\tzone vamos_%x {\n\t\t\tmatch {\n\t\t\t\tpath %s\n\t\t\t}\n", zone[:16], match)
		fmt.Fprintf(out, "\t\t\tkey {http.request.remote.host}\n\t\t\tevents %d\n\t\t\twindow %s\n\t\t}\n", *rule.Requests, window)
	}
	out.WriteString("\t}\n")
}
