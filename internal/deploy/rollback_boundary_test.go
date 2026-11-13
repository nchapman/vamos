package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestRollbackCaptureValidation(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(rollbackApp("1"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deploy(rollbackApp("2"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "web", true), nil); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(f.load())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*journal){
		"old version cannot carry rollback": func(j *journal) { j.Version = 4 },
		"missing capture":                   func(j *journal) { j.Rollback = nil },
		"source archive forbidden":          func(j *journal) { j.Archive = j.Operation + ".context.tar" },
		"source image map forbidden": func(j *journal) {
			j.BuildImages = map[string]string{strings.Repeat("a", 32): "sha256:" + strings.Repeat("b", 64)}
		},
		"unknown target":                   func(j *journal) { j.Rollback.Targets["unknown"] = j.Rollback.Targets["web"] },
		"nil target":                       func(j *journal) { j.Rollback.Targets["web"] = nil },
		"nil slot":                         func(j *journal) { j.ServiceStates["web"] = nil },
		"missing selected input":           func(j *journal) { delete(j.Rollback.Targets, "web") },
		"target image mismatch":            func(j *journal) { j.Rollback.Targets["web"].Image = "sha256:" + strings.Repeat("f", 64) },
		"manual target":                    func(j *journal) { j.Rollback.Targets["web"].Config.Update = config.UpdateManual },
		"nil current config":               func(j *journal) { j.ServiceStates["web"].Current.Config = nil },
		"different captured config":        func(j *journal) { j.Rollback.Targets["web"].Config.Command = []string{"changed"} },
		"extra volume identity":            func(j *journal) { j.Rollback.Targets["web"].VolumeID = strings.Repeat("a", 32) },
		"skipped selected service":         func(j *journal) { j.ServiceStates["web"].Skipped = true },
		"unselected service participating": func(j *journal) { j.ServiceStates["backend"].Skipped = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var j journal
			if err := json.Unmarshal(data, &j); err != nil {
				t.Fatal(err)
			}
			mutate(&j)
			if err := validateMultiJournal(&j, j.App); err == nil {
				t.Fatal("accepted corrupt rollback")
			}
		})
	}
}
func TestRollbackWithOnlyExcludedServicesIsSuccessfulNoOp(t *testing.T) {
	f := newMultiFlow(t)
	a := app()
	s := a.Services["web"]
	s.Update = config.UpdateManual
	a.Services["web"] = s
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	s.Env = map[string]string{"REVISION": "2"}
	a.Services["web"] = s
	q := f.req
	q.Config = a
	q.Service = "web"
	q.Apply = true
	q.Operation = strings.Repeat("2", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(a)); err != nil {
		t.Fatal(err)
	}
	before := f.load().Operation
	resp, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "", true), nil)
	if err != nil || resp.Status == nil || resp.RollbackPreview == nil || resp.RollbackPreview.NeedsConfirmation || f.load().Operation != before {
		t.Fatalf("excluded-only rollback: %+v %v", resp, err)
	}
}
