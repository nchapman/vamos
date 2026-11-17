package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/nchapman/vamos/internal/deploy"
)

func TestPublishedPortStatusPresentation(t *testing.T) {
	current := &deploy.PublishedPortStatus{Mappings: []string{"25565:25565", "27015-27020:27015-27020/udp"}, Total: 2, Observed: true}
	candidate := &deploy.PublishedPortStatus{Mappings: []string{"30000:25565/tcp"}, Total: 1}
	capped := &deploy.PublishedPortStatus{Total: 40}
	for i := 0; i < 32; i++ {
		capped.Mappings = append(capped.Mappings, fmt.Sprintf("%d:8080", 30000+i))
	}
	for _, tc := range []struct {
		name               string
		current, candidate *deploy.PublishedPortStatus
		want               []string
	}{
		{"mixed-http", current, nil, []string{"current published ports", "25565:25565", "27015-27020:27015-27020/udp", "bindings validated on running owned runtime", "Mappings listed: 2/2"}},
		{"different-candidate", current, candidate, []string{"current published ports", "candidate published ports", "30000:25565/tcp", "25565:25565"}},
		{"candidate-only", nil, candidate, []string{"candidate published ports", "30000:25565/tcp"}},
		{"stopped-unobserved", candidate, nil, []string{"not confirmation that ports are closed"}},
		{"capped", capped, nil, []string{"Mappings listed: 32/40 (truncated)", "30031:8080"}},
		{"legacy", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := deploy.ServiceStatus{Kind: "public", URL: "https://ledger.example.net", PublishedPorts: tc.current, CandidatePublishedPorts: tc.candidate}
			status := &deploy.Status{App: "ledger", Services: map[string]deploy.ServiceStatus{"web": ss}}
			path, _, deps := appFixture(t)
			deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
				return deploy.Response{Version: 1, Status: status}, nil
			}
			var plain, diagnostic bytes.Buffer
			if code := appCommand(context.Background(), "status", []string{"-f", path}, &plain, &diagnostic, deps); code != 0 {
				t.Fatalf("code=%d: %s", code, &diagnostic)
			}
			var jsonOut bytes.Buffer
			if code := appCommand(context.Background(), "status", []string{"-f", path, "--json"}, &jsonOut, &diagnostic, deps); code != 0 {
				t.Fatalf("JSON code=%d: %s", code, &diagnostic)
			}
			var decoded deploy.Status
			if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Services["web"], ss) {
				t.Fatalf("JSON status changed: %s", &jsonOut)
			}
			outputs := []string{plain.String()}
			for _, width := range []int{48, 120} {
				outputs = append(outputs, ansi.Strip(newHelpAppsTerminal(width, true).p.statusReport(status, presentationReportFixture())))
			}
			for _, out := range outputs {
				// Styled prose may wrap; assertions compare normalized whitespace.
				normalized := strings.Join(strings.Fields(out), " ")
				wants := append([]string{"https://ledger.example.net"}, tc.want...)
				if tc.current != nil || tc.candidate != nil {
					wants = append(wants, "0.0.0.0", "omitted protocol defaults to tcp", "reachability is not verified")
				}
				for _, want := range wants {
					if !strings.Contains(normalized, want) {
						t.Errorf("missing %q:\n%s", want, out)
					}
				}
				if tc.current == nil && strings.Contains(out, "current published ports") {
					t.Errorf("invented current bindings: %s", out)
				}
				if tc.candidate == nil && strings.Contains(out, "candidate published ports") {
					t.Errorf("invented candidate bindings: %s", out)
				}
			}
		})
	}
}
