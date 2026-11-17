package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestHelpUXScopeAndDiscoverability(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"help", "deploy"}} {
		var plain, diag bytes.Buffer
		rich := newHelpAppsTerminal(48, true)
		if Run(args, &plain, &diag, "test") != 0 || Run(args, rich, &diag, "test") != 0 || diag.Len() != 0 {
			t.Fatalf("help failed: %s", &diag)
		}
		wants := []string{"backup repository", "backup configure", "backup plan", "public HTTP, private, and direct-port", "root or passwordless sudo", "server inspect remains unelevated"}
		if len(args) > 1 {
			wants = []string{"services.<name>.ports", "services.<name>.http.waf", "services.<name>.http.cache", "services.<name>.http.compression", "direct ports bypass HTTP middleware", "stop-first"}
		}
		for _, text := range []string{plain.String(), ansi.Strip(rich.String())} {
			normalized := strings.Join(strings.Fields(text), " ")
			for _, want := range wants {
				if !strings.Contains(normalized, want) {
					t.Errorf("missing %q in %s", want, text)
				}
			}
		}
	}
}
