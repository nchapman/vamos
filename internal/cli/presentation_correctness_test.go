package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/nchapman/vamos/internal/deploy"
	"strings"
	"testing"
)

func TestUXCorrectness(t *testing.T) {
	for _, rich := range []bool{false, true} {
		for _, scenario := range []string{"mixed", "candidate", "selected", "missing", "unknown"} {
			t.Run(scenario+map[bool]string{false: "/plain", true: "/rich"}[rich], func(t *testing.T) {
				path, _, deps := appFixture(t)
				s := &deploy.Status{Operation: visualOperation, Services: map[string]deploy.ServiceStatus{
					"web": {Kind: "public"}, "worker": {Kind: "ports", Unavailable: true, Candidate: "candidate", CandidateInstances: []deploy.InstanceStatus{{Ordinal: 2}, {Ordinal: 3}}},
				}}
				response := deploy.Response{Version: 1, Status: s}
				var failure error
				if scenario != "mixed" {
					failure = errors.New("HTTP readiness failed")
					response.Error = failure.Error()
				}
				if scenario == "missing" {
					response.Status = nil
				}
				if scenario == "unknown" {
					response.Version = 0
				}
				deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
					return response, failure
				}
				var out, diag bytes.Buffer
				terminalOut, terminalDiag := newHelpAppsTerminal(240, false), newHelpAppsTerminal(240, false)
				args := []string{"-f", path, visualOperation}
				if scenario == "selected" {
					args = append(args, "--service", "web")
				}
				var text string
				if rich {
					appCommand(context.Background(), "resume", args, terminalOut, terminalDiag, deps)
					text = terminalOut.String() + terminalDiag.String()
				} else {
					appCommand(context.Background(), "resume", args, &out, &diag, deps)
					text = out.String() + diag.String()
				}
				switch scenario {
				case "mixed":
					if strings.Contains(text, `\n`) {
						t.Errorf("rendered literal newline escape: %s", text)
					}
					for _, want := range []string{"Public DNS and certificate", "Protocol readiness and firewall"} {
						if !strings.Contains(text, want) {
							t.Errorf("missing %q: %s", want, text)
						}
					}
					if strings.Contains(text, "no public HTTP") {
						t.Error(text)
					}
				case "candidate":
					if !strings.Contains(text, "--service worker --candidate --replica 2") {
						t.Error(text)
					}
				default:
					if strings.Contains(text, "vamos logs") {
						t.Error(text)
					}
					if scenario == "unknown" && strings.Contains(text, "Readiness failed.") {
						t.Error(text)
					}
				}
			})
		}
	}
}

func TestReadinessCommandOverridesDuplicateTargetFlags(t *testing.T) {
	r := presentationReportFixture()
	r.extraFlags = []string{"--service", "old", "--candidate", "--replica=9", "--timeout", "30s"}
	text := r.command("logs", "--service", "worker", "--candidate", "--replica", "2")
	for _, flag := range []string{"--service", "--candidate", "--replica"} {
		if strings.Count(text, flag) != 1 {
			t.Fatalf("duplicate %s: %s", flag, text)
		}
	}
	if !strings.Contains(text, "--timeout 30s") || strings.Contains(text, "old") || strings.Contains(text, "=9") {
		t.Fatal(text)
	}
}
