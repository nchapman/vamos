package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func preparedDestroyFixture(t *testing.T, count int) (*flowEngine, Request, *journal, *maintenance) {
	t.Helper()
	f, q := flowSetup(t)
	j := f.execute(q, "archive", false)
	original := f.m.Run
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 1 && a[1] == "ls" {
			return process.Result{}, nil
		}
		return original(c, n, a, b)
	}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%032x.%032x.image.iid", i+1, i+1)
		if err := os.WriteFile(filepath.Join(f.m.dir(j.App), name), []byte("sha256:"+strings.Repeat("9", 64)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	q.Config = nil
	q.Action = "destroy"
	q.Apply = true
	q.Operation = strings.Repeat("e", 32)
	w, err := f.m.planMaintenance(context.Background(), j, "destroy", q.Operation)
	if err != nil {
		t.Fatal(err)
	}
	w.OldCaddy, w.NewCaddy, err = f.m.destroyRoutes(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.m.persistMaintenance(j, w); err != nil {
		t.Fatal(err)
	}
	return f, q, j, w
}
func installPreparedDestroy(t *testing.T, f *flowEngine, j *journal, w *maintenance) {
	t.Helper()
	j.Maintenance = w
	j.Phase = "destroying"
	if err := validateJournal(j, j.App); err != nil {
		t.Fatal(err)
	}
	if err := f.m.save(j); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceManifestCorruptionFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "substitution", "symlink", "metadata", "extra", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			f, q, j, w := preparedDestroyFixture(t, 100)
			installPreparedDestroy(t, f, j, w)
			dir := f.m.maintenanceManifestDir(j.App)
			page := filepath.Join(dir, maintenancePageName(0))
			var err error
			switch mode {
			case "missing":
				err = os.Remove(page)
			case "corrupt":
				err = os.WriteFile(page, []byte("broken"), 0600)
			case "substitution":
				b, e := os.ReadFile(filepath.Join(dir, maintenancePageName(1)))
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(page, b, 0600)
			case "symlink":
				if err = os.Remove(page); err == nil {
					err = os.Symlink(filepath.Join(dir, maintenancePageName(1)), page)
				}
			case "metadata":
				h, e := f.m.maintenanceHeader(j)
				if e != nil {
					t.Fatal(e)
				}
				h.HostID = "another-host"
				b, _ := json.Marshal(h)
				err = atomic(filepath.Join(dir, "intent.json"), b, 0600)
			case "extra":
				err = os.WriteFile(filepath.Join(dir, "unexplained"), []byte("keep"), 0600)
			case "mixed":
				b, e := json.Marshal(j)
				if e != nil {
					t.Fatal(e)
				}
				err = atomic(filepath.Join(f.m.dir(j.App), "state.json"), b, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.m.load(j.App); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt managed state became absent/valid: %v", err)
			}
			before := len(f.events)
			q.Action = "resume"
			q.Apply = false
			if _, err = f.m.Execute(context.Background(), q, nil); err == nil {
				t.Fatal("corrupt authority resumed")
			}
			if len(f.events) != before {
				t.Fatal("external work before complete authority validation")
			}
			if _, err = os.Stat(filepath.Join(f.m.dir(j.App), w.Files[0].Name)); err != nil {
				t.Fatal("resource deleted with corrupt authority", err)
			}
		})
	}
}

func TestMaintenancePublicationResumeNeverExpands(t *testing.T) {
	f, q, j, w := preparedDestroyFixture(t, 500)
	// Simulates complete page publication followed by interruption before the
	// state.json rename. The app journal still says ready.
	loaded, err := f.m.load(j.App)
	if err != nil || loaded.Maintenance != nil {
		t.Fatal("preparation changed journal", err)
	}
	if err = f.m.maintenanceFence(Request{Action: "deploy", App: "other", Operation: strings.Repeat("f", 32)}); err == nil {
		t.Fatal("orphan publication did not fence host")
	}
	if !strings.Contains(err.Error(), j.App) || !strings.Contains(err.Error(), w.Operation) {
		t.Fatalf("lost client cannot discover the prepared operation from the refusal: %v", err)
	}
	late := strings.Repeat("f", 32) + ".image.iid"
	if err = os.WriteFile(filepath.Join(f.m.dir(j.App), late), []byte("sha256:"+strings.Repeat("8", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	q.Action = "resume"
	q.Apply = false
	resp, err := f.m.Execute(context.Background(), q, nil)
	if err == nil || resp.Status.Phase != "destroying" {
		t.Fatalf("new file absorbed or completion reported: %+v %v", resp.Status, err)
	}
	loaded, err = f.m.load(j.App)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Maintenance.Manifest.Root != w.Manifest.Root || len(loaded.Maintenance.Files) != len(w.Files) || !loaded.Maintenance.ResourcesDone {
		t.Fatal("captured intent changed or missing evidence barrier")
	}
	if _, err = os.Stat(filepath.Join(f.m.dir(j.App), late)); err != nil {
		t.Fatal("uncaptured file removed")
	}
	if err = os.Remove(filepath.Join(f.m.dir(j.App), late)); err != nil {
		t.Fatal(err)
	}
	resp, err = f.m.Execute(context.Background(), q, nil)
	if err != nil || resp.Status.Phase != "destroyed" {
		t.Fatal("exact resume failed", err)
	}
	if _, err = os.Stat(f.m.path("/var/lib/vamos/maintenance")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("maintenance namespace prevents host teardown", err)
	}
}

func TestMaintenanceInterruptedPreparation(t *testing.T) {
	for _, mode := range []string{"empty", "temporary", "preparing", "completed-gc"} {
		t.Run(mode, func(t *testing.T) {
			f, q, j, w := preparedDestroyFixture(t, 100)
			dir := f.m.maintenanceManifestDir(j.App)
			h, err := f.m.maintenanceHeader(j)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "preparing":
				h.Preparing = true
				h.Intent.Manifest = nil
				b, _ := json.Marshal(h)
				if err = atomic(filepath.Join(dir, "intent.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
				// A page write can have been interrupted; only this explicitly non-authorizing
				// preparation may discard partial pages and re-inventory.
				if err = os.WriteFile(filepath.Join(dir, maintenancePageName(1)), []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty", "temporary":
				entries, _ := os.ReadDir(dir)
				for _, entry := range entries {
					if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "temporary" {
					if err = os.WriteFile(filepath.Join(dir, ".deploy-interrupted"), []byte("partial initial header"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "completed-gc":
				// A durable completion receipt permits GC to resume even after page unlink.
				j.LastMaintenanceOperation = w.Operation
				j.LastMaintenanceAction = w.Action
				if err = f.m.save(j); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(filepath.Join(dir, maintenancePageName(0))); err != nil {
					t.Fatal(err)
				}
				if err = f.m.maintenanceFence(q); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("incomplete GC not recovered", err)
				}
				return
			}
			if _, err = f.m.Execute(context.Background(), q, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaintenanceManifestJournalDoesNotGrowWithHistory(t *testing.T) {
	f, q, j, w := preparedDestroyFixture(t, 1)
	// Remove only an uninstalled test preparation; now construct an intent larger
	// than the journal limit to isolate storage capacity from filesystem inventory.
	h, err := f.m.maintenanceHeader(j)
	if err != nil {
		t.Fatal(err)
	}
	h.Preparing = true
	h.Intent.Manifest = nil
	b, _ := json.Marshal(h)
	if err = atomic(filepath.Join(f.m.maintenanceManifestDir(j.App), "intent.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.preparedMaintenance(j, "destroy", q.Operation); err != nil {
		t.Fatal(err)
	}
	w.Manifest = nil
	w.Files = nil
	w.Plan.Archives = nil
	for i := 0; i < 20000; i++ {
		name := fmt.Sprintf("%032x.%032x.image.iid", i+1, i+1)
		w.Files = append(w.Files, plannedFile{Name: name, SHA256: strings.Repeat("a", 64)})
		w.Plan.Archives = append(w.Plan.Archives, name)
	}
	if err = boundMaintenancePreview(&w.Plan); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(w)
	if len(b) <= 2<<20 {
		t.Fatal("fixture does not exceed journal ceiling")
	}
	if err = f.m.persistMaintenance(j, w); err != nil {
		t.Fatal(err)
	}
	installPreparedDestroy(t, f, j, w)
	b, err = os.ReadFile(filepath.Join(f.m.dir(j.App), "state.json"))
	if err != nil || len(b) > 64<<10 {
		t.Fatalf("journal grows with history: %d %v", len(b), err)
	}
	loaded, err := f.m.load(j.App)
	if err != nil || len(loaded.Maintenance.Files) != 20000 {
		t.Fatal("large intent failed round trip", err)
	}
	entries, err := os.ReadDir(f.m.maintenanceManifestDir(j.App))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "intent.json" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maintenancePageBytes {
			t.Fatal("unbounded page", err)
		}
	}
	if w.Plan.Totals == nil || w.Plan.Totals.Archives != 20000 || !w.Plan.DetailsIncomplete {
		t.Fatal("prefix presented as exact full list")
	}
}
