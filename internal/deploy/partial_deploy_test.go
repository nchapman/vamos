package deploy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestPartialIndependentManifestsSharedBuild(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	first := &config.App{Name: f.req.App, Services: map[string]config.Service{"web": multiApp().Services["web"]}}
	if _, err := f.deploy(first, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load().ServiceStates["web"].Current
	// A separate directory declares only its service, with an identical build key.
	second := &config.App{Name: f.req.App, Services: map[string]config.Service{"worker": first.Services["web"]}}
	cfg := second.Services["worker"]
	cfg.HTTP = nil
	second.Services["worker"] = cfg
	builds := f.builds
	if _, err := f.deploy(second, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if f.builds != builds+1 || !reflect.DeepEqual(j.Selected, []string{"worker"}) || !j.ServiceStates["web"].Skipped || !reflect.DeepEqual(before, j.ServiceStates["web"].Current) {
		t.Fatalf("omitted service changed: %#v", j)
	}
	// Updating only worker must preserve web even though its build context aliases worker.
	f.req.Bindings = map[string]string{"worker": j.ServiceStates["worker"].Incarnation}
	f.req.HostIdentity = j.HostID
	cfg.Env = map[string]string{"REV": "next"}
	second.Services["worker"] = cfg
	if _, err := f.deploy(second, strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.load().ServiceStates["web"].Current) {
		t.Fatal("unselected release changed")
	}
	q := f.req
	q.Partial = false
	q.Operation = strings.Repeat("4", 32)
	q.Config = second
	if _, err := f.m.Execute(context.Background(), q, f.bundle(second)); err == nil {
		t.Fatal("v7 accepted legacy deployment")
	}
}

func TestPartialEffectiveUsesActualNotPendingConfig(t *testing.T) {
	actual := multiApp().Services["backend"]
	actual.Env = map[string]string{"REV": "actual"}
	pending := actual
	pending.Env = map[string]string{"REV": "pending"}
	old := &journal{Version: 7, Config: &config.App{Name: "notes", Services: map[string]config.Service{"backend": pending}}, ServiceStates: map[string]*serviceState{"backend": {Current: &release{Config: &actual}}}}
	local := &config.App{Name: "notes", Services: map[string]config.Service{"web": multiApp().Services["web"]}}
	got, err := effectiveConfig(Request{App: "notes", Partial: true, Config: local}, old)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Services["backend"], actual) {
		t.Fatal("copied pending desired config")
	}
	keep, err := keepService(Request{Partial: true, Config: local}, "backend", pending, nil, nil)
	if err != nil || !keep {
		t.Fatal("omitted service consulted staged secrets")
	}
}

func TestPartialLegacyJournalRejectsNewFields(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{2, 3, 4, 6} {
		j := f.load()
		j.Version = version
		j.Selected = []string{"web"}
		if validateMultiJournal(j, j.App) == nil {
			t.Fatalf("v%d accepted selection", version)
		}
		j.Selected = nil
		j.ServiceStates["web"].Previous = j.ServiceStates["web"].Current
		if validateMultiJournal(j, j.App) == nil {
			t.Fatalf("v%d accepted service history", version)
		}
	}
}

func TestPartialSupersedeExactSelection(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	f.failBuild = 1
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err == nil {
		t.Fatal("expected build failure")
	}
	q := f.req
	q.Operation = strings.Repeat("2", 32)
	q.Supersede = strings.Repeat("1", 32)
	q.Config = &config.App{Name: q.App, Services: map[string]config.Service{"web": multiApp().Services["web"]}}
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("wrong supersede result: %v", err)
	}
}

func TestPartialMergedServiceLimit(t *testing.T) {
	local := &config.App{Name: "notes", Services: map[string]config.Service{"extra": multiApp().Services["backend"]}}
	old := &journal{Version: 7, ServiceStates: map[string]*serviceState{}}
	for i := 0; i < MaxServices; i++ {
		cfg := multiApp().Services["backend"]
		old.ServiceStates[string(rune('a'+i))] = &serviceState{Current: &release{Config: &cfg}}
	}
	if _, err := effectiveConfig(Request{App: "notes", Partial: true, Config: local}, old); err == nil {
		t.Fatal("partial manifests bypassed merged service limit")
	}
}
