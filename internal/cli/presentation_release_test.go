package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func TestReleasePresentationShowsExecutionIdentityWithoutOutput(t *testing.T) {
	code := 7
	result := &deploy.RunResult{Operation: strings.Repeat("a", 32), Service: "web", Phase: "complete", ExitCode: &code, Output: "raw-output-must-not-appear"}
	for _, width := range []int{48, 100} {
		p := newHelpAppsTerminal(width, false).p
		s := presentationStatusFixture()
		s.Release = result
		text := p.statusReport(s, presentationReportFixture())
		for _, want := range []string{"Release command", "Run operation", result.Operation, "Exit code", "Rollout is blocked", "--run"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, result.Output) {
			t.Fatal("status disclosed command output")
		}
	}
	var plain bytes.Buffer
	if err := printReleaseSummary(&plain, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain.String(), "Release exit code: 7") || strings.Contains(plain.String(), result.Output) {
		t.Fatal(plain.String())
	}
}
