package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func incompleteMaintenancePlan() *deploy.Plan {
	return &deploy.Plan{Action: "destroy", App: "hello", DetailsIncomplete: true, Totals: &deploy.PlanTotals{Containers: 2, Archives: 5000, Retained: 1}}
}

func TestIncompleteMaintenancePreviewIsExplicit(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Archive = nil
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Plan: incompleteMaintenancePlan()}, nil
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"-f", path}
		if asJSON {
			args = append(args, "--json")
		}
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), "destroy", args, &out, &diagnostic, deps); code != 0 {
			t.Fatalf("%d: %s", code, &diagnostic)
		}
		if asJSON {
			var got deploy.Response
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Plan == nil || !got.Plan.DetailsIncomplete || got.Plan.Totals == nil || got.Plan.Totals.Archives != 5000 {
				t.Fatal("lost incomplete-detail metadata")
			}
		} else {
			for _, want := range []string{"Details are incomplete", "5000", "entire recomputed plan", "no resources deleted"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, &out)
				}
			}
		}
	}
}

func TestStyledIncompleteMaintenancePreviewDoesNotClaimEmpty(t *testing.T) {
	p := newHelpAppsTerminal(100, false).p
	r := presentationReportFixture()
	r.action = "destroy"
	r.mutation = true
	text := p.maintenanceReport(deploy.Response{Plan: incompleteMaintenancePlan()}, r)
	for _, want := range []string{"Details are incomplete", "5000", "entire recomputed plan", "none displayed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if regexp.MustCompile(`(?m)^(Containers|Images|Archives)\s+\(none\)$`).MatchString(text) || strings.Contains(text, "No retained resources listed in this plan") {
		t.Fatal("omitted details claimed absent:", text)
	}
}
