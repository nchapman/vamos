package deploy

import (
	"context"
	"encoding/json"
	"github.com/nchapman/vamos/internal/process"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceRouteProofAndFence(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "archive", false)
	old, next, e := f.m.destroyRoutes(j)
	if e != nil {
		t.Fatal(e)
	}
	op := strings.Repeat("e", 32)
	j.Maintenance = &maintenance{Operation: op, Action: "destroy", Plan: Plan{Action: "destroy", App: j.App}, OldCaddy: old, NewCaddy: next}
	j.Phase = "destroying"
	if e = f.m.save(j); e != nil {
		t.Fatal(e)
	}
	if e = f.m.maintenanceFence(Request{Action: "deploy", App: "other"}); e == nil {
		t.Fatal("missing host fence")
	}
	if e = f.m.maintenanceFence(Request{Action: "resume", App: j.App, Operation: op}); e != nil {
		t.Fatal(e)
	}
	f.reloadCrash = "before"
	if e = f.m.maintenanceRoutes(context.Background(), j, true); e == nil {
		t.Fatal("unproven reload accepted")
	}
	for _, event := range f.events {
		if strings.HasPrefix(event, "docker stop") || strings.HasPrefix(event, "docker rm") {
			t.Fatal("removed before proof")
		}
	}
	f.adminFail = false
	f.reloadCrash = ""
	if e = f.m.maintenanceRoutes(context.Background(), j, true); e != nil {
		t.Fatal(e)
	}
	if f.live != next {
		t.Fatal("route not removed")
	}
}

func TestMaintenanceMinimalReceiptAndRedeploy(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "archive", false)
	op := strings.Repeat("e", 32)
	// A completed destruction receipt must load without deployment configuration.
	b := `{"version":1,"app":"` + j.App + `","operation":"` + op + `","phase":"destroyed","HostID":"` + j.HostID + `"}`
	if e := atomic(filepath.Join(f.m.dir(j.App), "state.json"), []byte(b), 0600); e != nil {
		t.Fatal(e)
	}
	q := flowResume(r)
	q.Operation = op
	resp, e := f.m.Execute(context.Background(), q, nil)
	if e != nil {
		t.Fatal(e)
	}
	if resp.Plan == nil || resp.Plan.Action != "destroy" {
		t.Fatal("destroy resume reported a different operation kind")
	}
	if resp.Status.Phase != "destroyed" || resp.Status.Container != "" || resp.Status.URL == "" {
		t.Fatalf("bad receipt status %+v", resp.Status)
	}
	f.live = baseCaddy
	if e = atomic(f.m.path("/etc/caddy/Caddyfile"), []byte(baseCaddy), 0644); e != nil {
		t.Fatal(e)
	}
	r.Operation = strings.Repeat("f", 32)
	// The fake still has old resources; redeployment must nevertheless not retain
	// any release pointers from the receipt.
	j = f.execute(r, "new archive", false)
	if j.Previous != nil {
		t.Fatal("receipt retained old release")
	}
	if _, e = os.Stat(filepath.Join(f.m.dir(j.App), "state.json")); e != nil {
		t.Fatal(e)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "archive", false)
	j.Phase = "cleaning"
	j.Maintenance = &maintenance{Operation: strings.Repeat("e", 32), Action: "cleanup", Plan: Plan{Action: "cleanup", App: j.App}}
	if e := validateJournal(j, j.App); e != nil {
		t.Fatal(e)
	}
	j.Maintenance.Files = []plannedFile{{Name: "../state.json", SHA256: strings.Repeat("a", 64)}}
	if e := validateJournal(j, j.App); e == nil {
		t.Fatal("unsafe file accepted")
	}
}

func TestMaintenancePreviewCompletionAndResume(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "archive", false)
	original := f.m.Run
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 1 && a[1] == "ls" {
			return process.Result{}, nil
		}
		return original(c, n, a, b)
	}
	oldOp := j.Operation
	p := filepath.Join(f.m.dir(j.App), "state.json")
	before, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	q := r
	q.Config = nil
	q.Action = "cleanup"
	q.Operation = strings.Repeat("e", 32)
	resp, e := f.m.Execute(context.Background(), q, nil)
	if e != nil {
		t.Fatal(e)
	}
	if resp.Plan == nil {
		t.Fatal("no preview")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("preview wrote journal")
	}
	q.Apply = true
	if resp, e = f.m.Execute(context.Background(), q, nil); e != nil {
		t.Fatal(e)
	}
	if resp.Status.Operation != q.Operation {
		t.Fatal("cleanup response lost maintenance operation identity")
	}
	j, e = f.m.load(j.App)
	if e != nil {
		t.Fatal(e)
	}
	if j.Operation != oldOp || j.Maintenance != nil || j.Phase != "ready" {
		t.Fatal("cleanup changed deployment")
	}
	q.Action = "resume"
	q.Apply = false
	if _, e = f.m.Execute(context.Background(), q, nil); e != nil {
		t.Fatal(e)
	}
	q.Action = "destroy"
	q.Operation = strings.Repeat("f", 32)
	q.Apply = true
	if _, e = f.m.Execute(context.Background(), q, nil); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(f.m.dir(j.App))
	if e != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("not minimal: %v %v", entries, e)
	}
	q.Action = "resume"
	q.Apply = false
	resp, e = f.m.Execute(context.Background(), q, nil)
	if e != nil {
		t.Fatal(e)
	}
	if resp.Status.Phase != "destroyed" {
		t.Fatal("not destroyed")
	}
}

func TestMaintenanceDestroyRouteIsolation(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "archive", false)
	b, e := json.Marshal(j)
	if e != nil {
		t.Fatal(e)
	}
	var other journal
	if e = json.Unmarshal([]byte(strings.ReplaceAll(string(b), j.App, "other")), &other); e != nil {
		t.Fatal(e)
	}
	other.Current.Port = "14001"
	other.Current.PublishPort = "14001"
	if e = f.m.save(&other); e != nil {
		t.Fatal(e)
	}
	old, next, e := f.m.destroyRoutes(j)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(old, j.App+".") || strings.Contains(next, j.App+".") || !strings.Contains(next, "other.") || !strings.Contains(next, "127.0.0.1:14001") {
		t.Fatalf("route isolation failed: %q -> %q", old, next)
	}
	other.OldCaddy = old
	other.NewCaddy = next
	if e = f.m.save(&other); e != nil {
		t.Fatal(e)
	}
	if _, _, e = f.m.destroyRoutes(j); e == nil {
		t.Fatal("pending promotion did not fence destroy planning")
	}
}
