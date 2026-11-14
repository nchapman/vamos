package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMaintenanceResponseBudgetPreservesFailure(t *testing.T) {
	cause := errors.New("replacement container refused")
	resp := Response{Version: 1, Error: cause.Error(), Status: &Status{App: "hello", Operation: strings.Repeat("a", 32), Phase: "destroying", LastError: strings.Repeat("x", 70<<10)}, Plan: &Plan{Action: "destroy", App: "hello"}}
	got, err := boundMaintenanceResponse(resp, cause)
	if !errors.Is(err, cause) || !strings.Contains(got.Error, cause.Error()) || !strings.Contains(got.Error, "exceeds 64 KiB") {
		t.Fatalf("lost original refusal: %+v %v", got, err)
	}
	if got.Plan != nil || got.Status == nil || got.Status.App != "hello" || got.Status.Operation != resp.Status.Operation || got.Status.Phase != "destroying" || got.Status.LastError != "" {
		t.Fatalf("unsafe minimal projection: %+v", got)
	}
	b, e := json.Marshal(got)
	if e != nil || len(b)+1 > 64<<10 {
		t.Fatal("oversized response escaped", e)
	}
}

func TestMaintenanceResponseBudgetCannotReportOversizedSuccess(t *testing.T) {
	resp := Response{Version: 1, Status: &Status{App: "hello", Phase: "destroyed", LastError: strings.Repeat("x", 70<<10)}}
	got, err := boundMaintenanceResponse(resp, nil)
	if err == nil || got.Error == "" || got.Status.Phase != "destroyed" {
		t.Fatalf("ambiguous acknowledgement: %+v %v", got, err)
	}
	small := Response{Version: 1, Plan: &Plan{Action: "cleanup", App: "hello"}}
	got, err = boundMaintenanceResponse(small, nil)
	if err != nil || got.Plan != small.Plan || got.Error != "" {
		t.Fatal("small response changed")
	}
}
