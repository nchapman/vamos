package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestServiceHistoryIndependentPredecessors(t *testing.T) {
	o1, o2 := &release{Operation: "o1"}, &release{Operation: "o2"}
	b1, b2 := &release{Operation: "b1"}, &release{Operation: "b2"}
	j := &journal{Version: 7, Completed: &completedRelease{Services: map[string]*release{"orders": o1, "billing": b1}}, ServiceStates: map[string]*serviceState{"orders": {Current: o2}, "billing": {Current: b1}}}
	recordServiceHistory(j)
	j.Completed.Services["orders"] = o2
	j.ServiceStates["billing"].Current = b2
	recordServiceHistory(j)
	j.Completed.Services["billing"] = b2
	recordServiceHistory(j) // No-op must not replace either predecessor.
	if j.ServiceStates["orders"].Previous != o1 || j.ServiceStates["billing"].Previous != b1 {
		t.Fatal("service history was displaced by unrelated update or no-op")
	}
	if !retainedOperation(j, "o1") || !retainedOperation(j, "b1") {
		t.Fatal("cleanup lost independent predecessors")
	}
	// Successful rollback swaps predecessor to the stable release it replaced.
	j.ServiceStates["orders"].Current = o1
	recordServiceHistory(j)
	if j.ServiceStates["orders"].Previous != o2 {
		t.Fatal("rollback is not reversible")
	}
}

func TestServiceHistoryLegacyUnchanged(t *testing.T) {
	for v := 2; v <= 6; v++ {
		j := &journal{Version: v, Completed: &completedRelease{Services: map[string]*release{"web": {Operation: "old"}}}, ServiceStates: map[string]*serviceState{"web": {Current: &release{Operation: "new"}}}}
		recordServiceHistory(j)
		if j.ServiceStates["web"].Previous != nil {
			t.Fatalf("legacy v%d acquired new schema history", v)
		}
	}
}

func TestV7ScopedRollbackInterleavedHistory(t *testing.T) {
	f := newMultiFlow(t)
	deploy := func(a *config.App, op, service string) {
		t.Helper()
		q := f.req
		q.Partial = true
		if service != "" {
			a = &config.App{Name: a.Name, Services: map[string]config.Service{service: a.Services[service]}}
		}
		q.Config = a
		q.Operation = strings.Repeat(op, 32)
		q.Service = service
		if op != "1" {
			old := f.load()
			q.HostIdentity = old.HostID
			q.Bindings = map[string]string{}
			for name, slot := range old.ServiceStates {
				if service == "" || service == name {
					q.Bindings[name] = slot.Incarnation
				}
			}
		}
		if _, err := f.m.Execute(context.Background(), q, f.bundle(a)); err != nil {
			t.Fatal(err)
		}
	}
	deploy(rollbackApp("1"), "1", "")
	original := f.load()
	deploy(rollbackApp("2"), "2", "web")
	web2 := f.load().ServiceStates["web"].Current
	deploy(rollbackApp("3"), "3", "backend")
	stable := f.load()
	missing := *stable.ServiceStates["web"]
	stable.ServiceStates["web"].Previous = nil
	if _, _, err := rollbackPlan(rollbackRequest(f, "4", "web", true), stable); err == nil {
		t.Fatal("missing predecessor silently became no-op")
	}
	*stable.ServiceStates["web"] = missing
	next, _, err := rollbackPlan(rollbackRequest(f, "4", "web", true), stable)
	if err != nil {
		t.Fatal(err)
	}
	if next.Rollback.Targets["web"].Image != original.ServiceStates["web"].Current.Image || next.ServiceStates["backend"].Current.Image != stable.ServiceStates["backend"].Current.Image {
		t.Fatal("scoped rollback used app snapshot rather than service predecessor")
	}
	if next.Version != 7 || len(next.Selected) != 1 || next.Selected[0] != "web" || next.ServiceStates["web"].Incarnation != stable.ServiceStates["web"].Incarnation {
		t.Fatal("rollback lost v7 identities or selection")
	}
	appwide, _, err := rollbackPlan(rollbackRequest(f, "6", "", true), stable)
	if err != nil {
		t.Fatal(err)
	}
	if appwide.Rollback.Targets["web"].Image != web2.Image {
		t.Fatal("app-wide rollback lost snapshot semantics")
	}
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "4", "web", true), nil); err != nil {
		t.Fatal(err)
	}
	reversed := f.load()
	if reversed.ServiceStates["web"].Previous.Image != web2.Image {
		t.Fatal("successful rollback lost reverse target")
	}
	if reversed.ServiceStates["backend"].Current.Image != stable.ServiceStates["backend"].Current.Image {
		t.Fatal("rollback changed omitted service")
	}
	// A failed operation must restore its completed baseline, not Previous.
	stable.Phase = "updating"
	stable.ServiceStates["web"].Current = original.ServiceStates["web"].Current
	recovery, _, err := rollbackPlan(rollbackRequest(f, "5", "web", true), stable)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Rollback.Targets["web"] != web2 && recovery.Rollback.Targets["web"].Image != web2.Image {
		t.Fatal("failed update rollback skipped stable release")
	}
}
