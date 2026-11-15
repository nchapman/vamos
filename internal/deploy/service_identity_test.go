package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func identityFixture() *journal {
	return &journal{Version: 7, HostID: strings.Repeat("a", 32), Status: Status{App: "shop", Operation: strings.Repeat("1", 32), Phase: "ready"}, Config: &config.App{Name: "shop", Services: map[string]config.Service{"orders": {}}}, Selected: []string{"orders"}, ServiceStates: map[string]*serviceState{"orders": {Incarnation: strings.Repeat("2", 32)}}}
}
func identityNext() *journal {
	j := identityFixture()
	j.Operation = strings.Repeat("3", 32)
	j.ServiceStates["orders"].Incarnation = ""
	return j
}

func TestServiceIdentityRejectsUnboundAndStaleDeployments(t *testing.T) {
	old := identityFixture()
	for _, id := range []string{"", strings.Repeat("4", 32)} {
		r := Request{Partial: true, Operation: strings.Repeat("3", 32), Bindings: map[string]string{"orders": id}}
		if err := prepareServiceIdentities(r, old, identityNext()); err == nil {
			t.Fatalf("accepted binding %q", id)
		} else if ErrorCode(err) != CodeBindingRequired {
			t.Fatalf("binding refusal lost its classification: %v", err)
		}
	}
	r := Request{Partial: true, Operation: strings.Repeat("3", 32), Bindings: map[string]string{"orders": old.ServiceStates["orders"].Incarnation}}
	next := identityNext()
	if err := prepareServiceIdentities(r, old, next); err != nil {
		t.Fatal(err)
	}
	if next.ServiceStates["orders"].Incarnation != old.ServiceStates["orders"].Incarnation {
		t.Fatal("update changed identity")
	}
	r.Partial = false
	if err := prepareServiceIdentities(r, old, identityNext()); err == nil {
		t.Fatal("legacy client bypassed bindings")
	}
}
func TestRemovedServiceRequiresExplicitReAddAndNewIdentity(t *testing.T) {
	old := identityFixture()
	id := old.ServiceStates["orders"].Incarnation
	delete(old.ServiceStates, "orders")
	old.Removed = map[string]string{"orders": id}
	r := Request{Partial: true, Operation: strings.Repeat("3", 32), Bindings: map[string]string{"orders": id}}
	if err := prepareServiceIdentities(r, old, identityNext()); err == nil {
		t.Fatal("stale manifest resurrected service")
	}
	r.ReAdd = true
	next := identityNext()
	if err := prepareServiceIdentities(r, old, next); err != nil {
		t.Fatal(err)
	}
	if next.ServiceStates["orders"].Incarnation == id || next.Removed["orders"] != "" {
		t.Fatal("re-add retained removed incarnation")
	}
	if old.Removed["orders"] != id {
		t.Fatal("planning mutated old journal")
	}
}
func TestLegacyIdentityFreezesOnUpgrade(t *testing.T) {
	j := identityFixture()
	j.Version = 4
	j.Selected = nil
	j.ServiceStates["orders"].Incarnation = ""
	before := serviceIncarnation(j, "orders")
	upgradeServiceJournal(j)
	j.Operation = strings.Repeat("5", 32)
	if serviceIncarnation(j, "orders") != before {
		t.Fatal("upgrade changed observed identity")
	}
}
func TestLegacyBindingStatusPreservesAvailability(t *testing.T) {
	m, r := setup(t)
	j := saveRouteLegacy(t, m, r.App, "ready")
	r.Action = "status"
	r.Service = "web"
	resp := Response{Status: &Status{App: j.App, Phase: "ready", Running: true, Container: j.Current.Container}}
	m.enrichIdentityResponse(context.Background(), r, j.HostID, &resp)
	s := resp.Status.Services["web"]
	if !s.Available || s.Image != j.Current.Image || s.Operation != j.Current.Operation || s.Incarnation == "" {
		t.Fatalf("legacy identity enrichment lost effective status: %+v", s)
	}
}

func TestValidatePartialRequestBoundary(t *testing.T) {
	for _, r := range []Request{
		{Partial: true, Action: "status"},
		{Bindings: map[string]string{"orders": strings.Repeat("1", 32)}},
		{HostIdentity: "invalid"},
		{ReAdd: true, Partial: true, Action: "deploy", ContextFormat: BundleFormat},
		{AllowProtected: true, Action: "deploy"},
		{Action: "service-remove"},
	} {
		if err := ValidateRequest(r); err == nil {
			t.Fatalf("accepted invalid request %+v", r)
		}
	}
}
