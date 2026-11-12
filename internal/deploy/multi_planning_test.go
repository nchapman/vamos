package deploy

import (
	"strings"
	"testing"
)

func TestMultiPlanningInspectionFailurePersistsValidRetryableJournal(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	a := multiApp()
	web := a.Services["web"]
	web.Env = map[string]string{"REVISION": "two"}
	a.Services["web"] = web
	f.docker.lose["inspect"] = 1
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil {
		t.Fatal("inspection failure accepted")
	}
	j := f.load()
	if j.Phase != "building" || j.LastError == "" {
		t.Fatal("planning failure was not recorded in a valid journal")
	}
	for _, s := range j.ServiceStates {
		if s.Phase != "pending" || s.Candidate != nil {
			t.Fatal("partially planned candidates escaped building phase")
		}
	}
	if _, err := f.resume(j.Operation); err != nil {
		t.Fatal(err)
	}
}
