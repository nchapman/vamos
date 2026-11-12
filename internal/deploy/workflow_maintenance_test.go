package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestWorkflowMaintenanceRequiresDurableRunResult(t *testing.T) {
	m := NewManager()
	m.Root, _ = filepath.EvalSymlinks(t.TempDir())
	j := &journal{Version: 4, HostID: strings.Repeat("a", 32), Status: Status{App: "notes"}}
	if err := os.MkdirAll(m.dir(j.App), 0700); err != nil {
		t.Fatal(err)
	}
	op := strings.Repeat("b", 32)
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "web", Phase: "building", Container: "soar-run-" + op}, Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}

	r.Phase = "complete"
	r.Image = "sha256:" + strings.Repeat("c", 64)
	r.DockerID = strings.Repeat("d", 64)
	code := 0
	r.ExitCode = &code
	if err := m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	if err := m.maintenanceRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runPath(m, j, op), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.maintenanceRuns(context.Background(), j); runtime.GOOS != "windows" && err == nil {
		t.Fatal("public record admitted")
	}
}

func TestWorkflowMaintenanceLegacyMountBoundary(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4} {
		if supportsVolumes(&journal{Version: version}) != (version >= 3) {
			t.Fatalf("version %d", version)
		}
	}
}

func TestWorkflowMaintenancePrunesBoundedHistory(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	code := 0
	for i := 0; i < 256; i++ {
		op := fmt.Sprintf("%032x", i+100)
		r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "worker", Phase: "complete", Container: "soar-run-" + op, Image: "sha256:" + strings.Repeat("c", 64), ExitCode: &code, Output: "saved output"}, DockerID: strings.Repeat("d", 64), Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
		if err := f.m.saveRun(j, r); err != nil {
			t.Fatal(err)
		}
	}

	prune, err := f.m.maintenanceRunPrune(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(prune) != 32 {
		t.Fatal(len(prune))
	}
	for name := range prune {
		if err := os.Remove(filepath.Join(f.m.dir(j.App), name)); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(f.m.dir(j.App), "*.run.json"))
	if len(paths) != 224 {
		t.Fatal(len(paths))
	}

}

func TestWorkflowMaintenanceFailedBuild(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	op := strings.Repeat("b", 32)
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "worker", Phase: "building", Container: "soar-run-" + op}, Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := f.m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}

	if err := f.m.preflightRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err := f.m.maintenanceRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	prune, err := f.m.maintenanceRunPrune(j)
	if err != nil {
		t.Fatal(err)
	}
	if !prune[op+".run.json"] {
		t.Fatal("prestart record retained")
	}
	if err := os.Remove(runPath(f.m, j, op)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(runPath(f.m, j, op)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := f.m.preflightRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}
