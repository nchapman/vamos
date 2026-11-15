package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestMaintenanceDistinctRetainedPreview(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	for i := 0; i < 1500; i++ {
		id := fmt.Sprintf("sha256:%064x", i+1)
		image := &resourceInspection{RepoTags: []string{"shared:retained"}}
		image.ID = id
		f.images[id] = image
		f.file(t, fmt.Sprintf("%032x.%032x.image.iid", i+1, i+1), id+"\n")
	}
	var prior []byte
	for i := 0; i < 2; i++ {
		w, err := f.m.planMaintenance(context.Background(), f.j, "cleanup", strings.Repeat("e", 32))
		if err != nil {
			t.Fatal(err)
		}
		if !w.Plan.DetailsIncomplete || w.Plan.Totals == nil || w.Plan.Totals.Retained != 1500 || len(w.Files) != 0 || len(w.Images) != 0 {
			t.Fatalf("incorrect retained inventory: %+v", w.Plan.Totals)
		}
		b, err := json.Marshal(w.Plan)
		if err != nil || len(b) > maxMaintenancePlanBytes {
			t.Fatal("unbounded preview", err)
		}
		if i > 0 && string(prior) != string(b) {
			t.Fatal("preview depends on inventory map order")
		}
		prior = b
	}
}

// Represents a crash after a verified prestart run container was removed, but
// before Docker's removal response or file deletion. Its exact captured proof
// must survive page separation, reload, and a lost progress write. The same
// exercise for inline authority protects the legacy recovery path.
func TestMaintenanceRunRemovalProofAcrossPagesAndInline(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%v", paged), func(t *testing.T) {
			f, q := flowSetup(t)
			j := f.execute(q, "archive", false)
			original := f.m.Run
			f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
				if n == "docker" && len(a) > 1 && a[1] == "ls" {
					return process.Result{}, nil
				}
				return original(c, n, a, b)
			}
			image := "sha256:" + strings.Repeat("9", 64)
			op := strings.Repeat("d", 32)
			r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "web", Phase: "create-intent", Container: "vamos-run-" + op, Image: image}, DockerID: strings.Repeat("8", 64), Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
			if err := f.m.saveRun(j, r); err != nil {
				t.Fatal(err)
			}
			_, hash, err := resourceRead(runPath(f.m, j, op), runRecordLimit)
			if err != nil {
				t.Fatal(err)
			}
			w := &maintenance{Action: "cleanup", Operation: strings.Repeat("e", 32), Plan: Plan{Action: "cleanup", App: j.App}}
			for i := 0; i < 40; i++ {
				o := fmt.Sprintf("%032x", i+1)
				w.Containers = append(w.Containers, plannedContainer{Name: containerName(j.App, "", o), ID: fmt.Sprintf("%064x", i+1), Operation: o, Image: image, NetworkID: strings.Repeat("f", 64)})
			}
			w.Containers = append(w.Containers, plannedContainer{Name: r.Container, ID: r.DockerID, Operation: op, Service: r.Service, Image: r.Image, NetworkID: strings.Repeat("f", 64)})
			w.Files = []plannedFile{{Name: op + ".run.json", SHA256: hash}}
			w.Plan.Archives = []string{op + ".run.json"}
			for _, c := range w.Containers {
				w.Plan.Containers = append(w.Plan.Containers, c.Name)
			}
			if paged {
				if err = f.m.persistMaintenance(j, w); err != nil {
					t.Fatal(err)
				}
			}
			j.Maintenance = w
			j.Phase = "cleaning"
			if err = validateJournal(j, j.App); err != nil {
				t.Fatal(err)
			}
			if err = f.m.save(j); err != nil {
				t.Fatal(err)
			}
			j, err = f.m.load(j.App)
			if err != nil {
				t.Fatal(err)
			}
			if !f.m.plannedRunRemoval(j, r) {
				t.Fatal("lost immutable cross-page run proof")
			}
			if err = f.m.preflightRuns(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			if err = f.m.maintenanceRuns(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			// Same operation/container but a different record is not sufficient proof.
			r.Output = "changed"
			if err = f.m.saveRun(j, r); err != nil {
				t.Fatal(err)
			}
			if f.m.plannedRunRemoval(j, r) || f.m.preflightRuns(context.Background(), j) == nil {
				t.Fatal("changed record accepted as removal evidence")
			}
			r.Output = ""
			if err = f.m.saveRun(j, r); err != nil {
				t.Fatal(err)
			}
			if err = f.m.removeMaintenanceResources(context.Background(), j, j.Maintenance); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(f.m.dir(j.App), op+".run.json")); !os.IsNotExist(err) {
				t.Fatal("run record not removed", err)
			}
		})
	}
}

func TestMaintenanceCleanupPreservesPrunedOperationReplayFence(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	original := f.m.Run
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 1 && a[1] == "ls" {
			return process.Result{}, nil
		}
		return original(c, n, a, b)
	}
	code := 0
	for i := 0; i < 33; i++ {
		op := fmt.Sprintf("%032x", i+100)
		r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "worker", Phase: "complete", Container: "vamos-run-" + op, Image: "sha256:" + strings.Repeat("9", 64), ExitCode: &code}, DockerID: strings.Repeat("8", 64), Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
		if err := f.m.saveRun(j, r); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.m.dir(j.App), op+"."+imageKey(r.Config)+".image.iid"), []byte(r.Image+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prune, err := f.m.maintenanceRunPrune(j)
	if err != nil || len(prune) != 1 {
		t.Fatal(prune, err)
	}
	var pruned string
	for name := range prune {
		pruned = strings.TrimSuffix(name, ".run.json")
	}
	q := f.req
	q.Action = "cleanup"
	q.Config = nil
	q.Apply = true
	q.Operation = strings.Repeat("e", 32)
	if _, err = f.m.executeMaintenance(context.Background(), q, j); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(runPath(f.m, j, pruned)); !os.IsNotExist(err) {
		t.Fatal("terminal record not pruned", err)
	}
	q.Action = "run"
	q.Operation = pruned
	if _, err = f.m.executeRun(context.Background(), q, nil, j, &lifecycle.State{ID: j.HostID}, nil); err == nil || !strings.Contains(err.Error(), "image provenance already exists") {
		t.Fatal("pruned run could execute again", err)
	}
	if _, err = f.deploy(multiApp(), pruned); err == nil || !strings.Contains(err.Error(), "operation ID already used") {
		t.Fatal("receipt did not fence deployment reuse", err)
	}
}
