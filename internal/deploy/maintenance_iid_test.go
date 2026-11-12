package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceRejectsUnexplainedEmptyIID(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	j := f.j
	name := strings.Repeat("2", 32) + "." + strings.Repeat("4", 32) + ".image.iid"
	if err := os.WriteFile(filepath.Join(f.m.dir(j.App), name), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.planMaintenance(context.Background(), j, "cleanup", strings.Repeat("3", 32)); err == nil {
		t.Fatal("accepted unexplained empty image provenance")
	}
}

func TestMaintenanceRemovesEmptyIIDOnlyForUnstartedBuild(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	j := f.j
	op := strings.Repeat("2", 32)
	cfg := *multiMaintenanceRelease("web", op, "sha256:"+strings.Repeat("c", 64), false).Config
	cfg.HTTP = nil
	cfg.Command = []string{"true"}
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "web", Phase: "building", Container: "soar-run-" + op}, Config: cfg}
	if err := f.m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	name := op + "." + imageKey(cfg) + ".image.iid"
	if err := os.WriteFile(filepath.Join(f.m.dir(j.App), name), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := f.m.planMaintenance(context.Background(), j, "cleanup", strings.Repeat("3", 32))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range w.Files {
		if file.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatal("empty failed-build artifact would outlive its provenance record")
	}
}
