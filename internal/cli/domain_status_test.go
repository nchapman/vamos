package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func TestPrintDomainStatuses(t *testing.T) {
	for _, tc := range []struct{ dns, cert, route, want string }{
		{"unknown", "unknown", "configured", "Run status"},
		{"pending", "pending", "configured", "DNS resolution is pending"},
		{"unverified", "ready", "configured", "NAT/CDN"},
		{"matches-host", "ready", "configured", "does not guarantee public reachability"},
		{"unknown", "unknown", "pending", "not configured yet"},
	} {
		var b bytes.Buffer
		err := printDomainStatuses(&b, []deploy.DomainStatus{{Hostname: "app.example.com", DNS: tc.dns, Certificate: tc.cert, Route: tc.route, HTTPS: "pending"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "https://app.example.com") || !strings.Contains(b.String(), tc.want) {
			t.Fatal(b.String())
		}
	}
}
