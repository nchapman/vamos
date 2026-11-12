package deploy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A full history leaves 256 durable IID receipts even after explicit cleanup.
func TestMaintenanceCapacityReceipts(t *testing.T) {
	f, req := flowSetup(t)
	j := f.execute(req, "archive", false)
	j.Phase = "destroying"
	w := &maintenance{Operation: strings.Repeat("e", 32), Action: "destroy", Plan: Plan{Action: "destroy", App: j.App}}
	for i := 0; i < 256; i++ {
		name := fmt.Sprintf("%032x.image.iid", i+1)
		w.Files = append(w.Files, plannedFile{Name: name, SHA256: strings.Repeat("a", 64)})
		w.Plan.Archives = append(w.Plan.Archives, name)
	}
	w.OldCaddy, w.NewCaddy, _ = f.m.destroyRoutes(j)
	j.Maintenance = w
	if err := validateJournal(j, j.App); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Response{Version: 1, Plan: &w.Plan})
	if err != nil || len(b) >= 64<<10 {
		t.Fatalf("response bytes=%d error=%v", len(b), err)
	}
	w.Plan.Retained = append(w.Plan.Retained, strings.Repeat("x", 33<<10))
	if err := validateJournal(j, j.App); err == nil {
		t.Fatal("oversized public plan accepted")
	}
}

// Distinct-image batches include retained image explanations for all deferred
// containers, not just the 32 images and records being removed.
func TestMaintenanceCapacityDistinctImageBatch(t *testing.T) {
	p := Plan{Action: "cleanup", App: "migration-app"}
	for i := 0; i < 256; i++ {
		id := fmt.Sprintf("sha256:%064x", i+1)
		if i < 32 {
			p.Images = append(p.Images, id)
			p.Containers = append(p.Containers, fmt.Sprintf("soar-run-%032x", i+1))
			p.Archives = append(p.Archives, fmt.Sprintf("%032x.run.json", i+1))
		} else {
			p.Retained = append(p.Retained, id+": referenced")
		}
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > maxMaintenancePlanBytes {
		t.Fatalf("batch bytes=%d error=%v", len(b), err)
	}
	t.Logf("256 distinct-image batch public plan: %d bytes", len(b))
}
