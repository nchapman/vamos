package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func domainsFlowApp(revision string, aliases ...string) *config.App {
	a := changedMultiApp(revision)
	s := a.Services["web"]
	s.HTTP.Domains = aliases
	a.Services["web"] = s
	return a
}

func domainsFlowRoutes(t *testing.T, f *multiFlow, present, absent []string) {
	t.Helper()
	for _, h := range present {
		if !strings.Contains(string(f.live), h) {
			t.Fatalf("missing route %s: %s", h, f.live)
		}
	}
	for _, h := range absent {
		if strings.Contains(string(f.live), h) {
			t.Fatalf("unexpected route %s: %s", h, f.live)
		}
	}
}

func domainsFlowOther(f *multiFlow, a *config.App, op string) (Response, error) {
	q := f.req
	q.App = "other"
	a.Name = q.App
	q.Config = a
	q.Operation = strings.Repeat(op, 32)
	return f.m.Execute(context.Background(), q, f.bundle(a))
}

func TestDomainsFlowPromotionAndRollback(t *testing.T) {
	f := newMultiFlow(t)
	resp, err := f.deploy(domainsFlowApp("one", "A.Custom.test", "Second.Custom.test"), strings.Repeat("1", 32))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status.URL != "https://notes.example.com" || f.load().Version != 6 {
		t.Fatalf("generated URL/schema changed: %+v", resp.Status)
	}
	domainsFlowRoutes(t, f, []string{"notes.example.com", "a.custom.test", "second.custom.test"}, []string{"A.Custom.test", "Second.Custom.test"})
	if _, err = f.deploy(domainsFlowApp("two", "b.custom.test"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	domainsFlowRoutes(t, f, []string{"notes.example.com", "b.custom.test"}, []string{"a.custom.test", "second.custom.test"})
	if _, err = f.m.Execute(context.Background(), rollbackRequest(f, "3", "", true), nil); err != nil {
		t.Fatal(err)
	}
	if f.load().Version != 6 {
		t.Fatal("rollback lost domain journal schema")
	}
	domainsFlowRoutes(t, f, []string{"notes.example.com", "a.custom.test", "second.custom.test"}, []string{"b.custom.test"})
}

func TestDomainsFlowFailedReadinessPreservesEffectiveAliases(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(domainsFlowApp("one", "a.custom.test"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := string(f.live)
	old := f.load().ServiceStates["web"].Current.Container
	f.failHealth["web"] = true
	if _, err := f.deploy(domainsFlowApp("two", "b.custom.test"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("accepted failed readiness")
	}
	if string(f.live) != before || f.load().ServiceStates["web"].Current.Container != old || !f.docker.containers[old].State.Running {
		t.Fatal("failed readiness changed effective deployment")
	}
	f.failHealth["web"] = false
	if _, err := f.resume(strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	domainsFlowRoutes(t, f, []string{"b.custom.test"}, []string{"a.custom.test"})
}

func TestDomainsFlowPendingClaimsBlockOtherApps(t *testing.T) {
	for _, alias := range []string{"claimed.custom.test", "other.example.com"} {
		t.Run(alias, func(t *testing.T) {
			f := newMultiFlow(t)
			f.failBuild = 1
			if _, err := f.deploy(domainsFlowApp("one", alias), strings.Repeat("1", 32)); err == nil {
				t.Fatal("expected interrupted build")
			}
			f.failBuild = 0
			other := domainsFlowApp("other")
			if alias != "other.example.com" {
				other = domainsFlowApp("other", alias)
			}
			events, live := len(f.docker.events), string(f.live)
			if _, err := domainsFlowOther(f, other, "2"); err == nil || !strings.Contains(err.Error(), "hostname") {
				t.Fatalf("expected hostname conflict: %v", err)
			}
			if len(f.docker.events) != events || string(f.live) != live {
				t.Fatal("conflict mutated runtime")
			}
		})
	}
}

func TestDomainsFlowReleasedAliasClaimBlocksRollbackBeforeMutation(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(domainsFlowApp("one", "a.custom.test"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deploy(domainsFlowApp("two", "b.custom.test"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	// Stop after reservation and journal persistence: this fixture models one
	// Docker network, and a pending deployment already owns its desired aliases.
	f.failBuild = f.builds + 1
	if _, err := domainsFlowOther(f, domainsFlowApp("other", "a.custom.test"), "3"); err == nil || !strings.Contains(err.Error(), "test build failed") {
		t.Fatalf("historical alias was not available for the other app: %v", err)
	}
	f.failBuild = 0
	events, live := len(f.docker.events), string(f.live)
	before := workflowFiles(t, f.m.dir(f.req.App))
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "4", "", true), nil); err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("expected rollback conflict: %v", err)
	}
	if len(f.docker.events) != events || string(f.live) != live {
		t.Fatal("rollback conflict mutated runtime")
	}
	after := workflowFiles(t, f.m.dir(f.req.App))
	if len(before) != len(after) {
		t.Fatal("rollback conflict changed journal files")
	}
	for name, b := range before {
		if string(after[name]) != string(b) {
			t.Fatalf("rollback conflict changed %s", name)
		}
	}
}

func TestDomainsFlowVolumeDowntimeRoutesUnionThenActivates(t *testing.T) {
	f := newMultiFlow(t)
	makeApp := func(revision, alias string) *config.App {
		a := domainsFlowApp(revision, alias)
		s := a.Services["web"]
		s.Volume = &config.Volume{Name: "web-data", Mount: "/data"}
		a.Services["web"] = s
		return a
	}
	if _, err := f.deploy(makeApp("one", "a.custom.test"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.failHealth["web"] = true
	if _, err := f.deploy(makeApp("two", "b.custom.test"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected readiness failure")
	}
	domainsFlowRoutes(t, f, []string{"notes.example.com", "a.custom.test", "b.custom.test", "503"}, nil)
	f.failHealth["web"] = false
	if _, err := f.resume(strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	domainsFlowRoutes(t, f, []string{"notes.example.com", "b.custom.test"}, []string{"a.custom.test", "503"})
}
