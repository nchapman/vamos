package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func planFixture(t *testing.T) (*Manager, string, Plan, PlanRuntime) {
	t.Helper()
	m, f, request, credentials, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, request, credentials); err != nil {
		t.Fatal(err)
	}
	calls := [3]int{f.inits, f.probes, f.checks}
	t.Cleanup(func() {
		if calls != [3]int{f.inits, f.probes, f.checks} {
			t.Error("plan contacted repository backend")
		}
	})
	p := Plan{App: "journal", Sources: map[string]Source{
		"db":      {Type: "sqlite", Service: "web", Volume: "data", Path: "db.sqlite"},
		"uploads": {Type: "files", Service: "web", Volume: "data", Path: "uploads"},
	}, Sets: map[string]Set{"daily": {Sources: []string{"uploads", "db"}, Consistency: "quiesced", Stop: []string{"web"}, Repository: "offsite"}}}
	rt := PlanRuntime{HostID: host, App: p.App, Services: map[string]PlanService{"web": {Incarnation: strings.Repeat("1", 32), Replicas: 1, Volume: &PlanVolume{ID: strings.Repeat("2", 32), Name: "data", Mount: "/data"}}}}
	return m, host, p, rt
}
func previewPlan(t *testing.T, m *Manager, host string, p Plan, rt PlanRuntime) PlanResponse {
	t.Helper()
	r, e := m.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "preview", Plan: &p}, rt)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func approval(p *Plan, r PlanResponse) PlanRequest {
	return PlanRequest{Action: "apply", Plan: p, ExpectedRevision: r.CurrentRevision, ExpectedDigest: r.Digest}
}

func TestExecutePlanPreviewApplyReplayAndShow(t *testing.T) {
	m, host, p, rt := planFixture(t)
	before, _ := json.Marshal(struct {
		P Plan
		R PlanRuntime
	}{p, rt})
	empty, e := m.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "show"}, PlanRuntime{})
	if e != nil || empty.Current != nil || empty.CurrentRevision != "none" {
		t.Fatalf("empty show: %+v %v", empty, e)
	}
	preview := previewPlan(t, m, host, p, rt)
	if _, e := os.Stat(m.planDirectory().dir); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("preview wrote state: %v", e)
	}
	if preview.Applied || preview.Current != nil || preview.Proposed.Bindings.Sources["db"].Volume.ID != rt.Services["web"].Volume.ID {
		t.Fatal("incorrect preview")
	}
	applied, e := m.ExecutePlan(context.Background(), host, p.App, approval(&p, preview), rt)
	if e != nil || !applied.Applied {
		t.Fatal(e)
	}
	after, _ := json.Marshal(struct {
		P Plan
		R PlanRuntime
	}{p, rt})
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	svc := rt.Services["web"]
	svc.SecretEnvs = []string{"UNRELATED_SECRET"}
	rt.Services["web"] = svc
	replay, e := m.ExecutePlan(context.Background(), host, p.App, approval(&p, preview), rt)
	if e != nil || !reflect.DeepEqual(applied.Current, replay.Current) {
		t.Fatalf("stable replay: %v", e)
	}
	fresh := &Manager{Root: m.Root}
	show, e := fresh.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "show"}, PlanRuntime{})
	if e != nil || !reflect.DeepEqual(show.Current, applied.Current) {
		t.Fatalf("durable show: %v", e)
	}
	encoded, _ := json.Marshal(show)
	for _, s := range []string{"UNRELATED_SECRET", "private-storage-key", "EXAMPLEACCESSKEY"} {
		if strings.Contains(string(encoded), s) {
			t.Fatal("captured runtime/secret", s)
		}
	}
}

func TestExecutePlanReplacementAndConflict(t *testing.T) {
	m, host, p, rt := planFixture(t)
	first := previewPlan(t, m, host, p, rt)
	if _, e := m.ExecutePlan(context.Background(), host, p.App, approval(&p, first), rt); e != nil {
		t.Fatal(e)
	}
	delete(p.Sources, "uploads")
	set := p.Sets["daily"]
	set.Sources = []string{"db"}
	p.Sets = map[string]Set{"replacement": set}
	next := previewPlan(t, m, host, p, rt)
	stale := approval(&p, next)
	stale.ExpectedRevision = "none"
	if _, e := m.ExecutePlan(context.Background(), host, p.App, stale, rt); !errors.Is(e, errPlanConflict) {
		t.Fatalf("stale: %v", e)
	}
	invalid := p
	invalid.App = "other"
	if _, e := m.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "preview", Plan: &invalid}, rt); e == nil {
		t.Fatal("invalid accepted")
	}
	old, e := m.loadPlan(p.App, host)
	if e != nil || old.Revision != first.Digest {
		t.Fatal("failed request replaced old", e)
	}
	result, e := m.ExecutePlan(context.Background(), host, p.App, approval(&p, next), rt)
	if e != nil || len(result.Current.Plan.Sources) != 1 || len(result.Current.Plan.Sets) != 1 {
		t.Fatal("not full replacement", e)
	}
}

func TestExecutePlanChangedBindingsRefused(t *testing.T) {
	for _, kind := range []string{"incarnation", "volume", "repository"} {
		t.Run(kind, func(t *testing.T) {
			m, host, p, rt := planFixture(t)
			pre := previewPlan(t, m, host, p, rt)
			switch kind {
			case "incarnation":
				s := rt.Services["web"]
				s.Incarnation = strings.Repeat("3", 32)
				rt.Services["web"] = s
			case "volume":
				rt.Services["web"].Volume.ID = strings.Repeat("3", 32)
			case "repository":
				r, e := m.storage().load("offsite", host)
				if e != nil {
					t.Fatal(e)
				}
				r.Metadata.RepositoryID = strings.Repeat("e", 64)
				if e = os.Remove(filepath.Join(m.storage().dir, "offsite.json")); e != nil {
					t.Fatal(e)
				}
				if e = m.storage().save(r); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := m.ExecutePlan(context.Background(), host, p.App, approval(&p, pre), rt); !errors.Is(e, errPlanConflict) {
				t.Fatalf("changed binding: %v", e)
			}
			if r, e := m.loadPlan(p.App, host); e != nil || r != nil {
				t.Fatal("failed apply wrote state", e)
			}
		})
	}
}

func TestExecutePlanPostgresSelection(t *testing.T) {
	for _, kind := range []string{"valid", "replicated", "missing-selector", "wrong-case"} {
		t.Run(kind, func(t *testing.T) {
			m, host, p, rt := planFixture(t)
			p.Sources["pg"] = Source{Type: "postgres", Service: "postgres", Database: "journal", User: "journal", Credentials: &CredentialSelector{Service: "data", Env: "PGPASSWORD"}, Restore: &RestoreProfile{Profile: "single-owner"}}
			set := p.Sets["daily"]
			set.Sources = append(set.Sources, "pg")
			set.Stop = append(set.Stop, "data")
			p.Sets["daily"] = set
			rt.Services["postgres"] = PlanService{Incarnation: strings.Repeat("3", 32), Replicas: 1}
			rt.Services["data"] = PlanService{Incarnation: strings.Repeat("4", 32), Replicas: 1, SecretEnvs: []string{"PGPASSWORD"}}
			switch kind {
			case "replicated":
				s := rt.Services["postgres"]
				s.Replicas = 2
				rt.Services["postgres"] = s
			case "missing-selector", "wrong-case":
				s := rt.Services["data"]
				s.SecretEnvs = nil
				if kind == "wrong-case" {
					s.SecretEnvs = []string{"pgpassword"}
				}
				rt.Services["data"] = s
			}
			r, e := m.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "preview", Plan: &p}, rt)
			if kind != "valid" {
				if !errors.Is(e, errPlanSelection) {
					t.Fatalf("selection: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			b := r.Proposed.Bindings.Sources["pg"]
			if b.CredentialService != "data" || b.CredentialIncarnation != rt.Services["data"].Incarnation || b.Volume != nil || r.Proposed.Bindings.Stops["data"] != b.CredentialIncarnation || r.Proposed.Plan.Sources["pg"].Port != 5432 || p.Sources["pg"].Port != 0 {
				t.Fatal("incorrect mixed binding/normalization")
			}
		})
	}
}

func TestExecutePlanRepositoryReadiness(t *testing.T) {
	for _, kind := range []string{"missing", "unverified", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			m, h, p, rt := planFixture(t)
			r, e := m.storage().load("offsite", h)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "missing":
				e = os.Remove(filepath.Join(m.storage().dir, "offsite.json"))
			case "unverified":
				r.Metadata.State = "unverified"
				e = m.storage().save(r)
			case "foreign":
				h = strings.Repeat("f", 32)
				rt.HostID = h
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = m.ExecutePlan(context.Background(), h, p.App, PlanRequest{Action: "preview", Plan: &p}, rt); !errors.Is(e, errPlanRepository) {
				t.Fatalf("repository selection: %v", e)
			}
		})
	}
}
func TestPlanRequestStrictSelection(t *testing.T) {
	m, h, p, rt := planFixture(t)
	for _, r := range []PlanRequest{
		{Action: "show", Plan: &p}, {Action: "show", ExpectedRevision: "none"},
		{Action: "preview", Plan: &p, ExpectedDigest: strings.Repeat("a", 64)},
		{Action: "apply", Plan: &p}, {Action: "capture", Plan: &p},
	} {
		if _, e := m.ExecutePlan(context.Background(), h, p.App, r, rt); !errors.Is(e, errPlanInput) {
			t.Errorf("mixed request %+v: %v", r, e)
		}
	}
}

func TestNineWriterPlan(t *testing.T) {
	m, host, p, rt := planFixture(t)
	set := p.Sets["daily"]
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("writer-%d", i)
		rt.Services[name] = PlanService{Incarnation: fmt.Sprintf("%032x", i), Replicas: 1}
		set.Stop = append(set.Stop, name)
	}
	p.Sets["daily"] = set
	preview := previewPlan(t, m, host, p, rt)
	if len(preview.Proposed.Bindings.Stops) != 9 {
		t.Fatal("missing writer identities")
	}
	if _, err := m.ExecutePlan(context.Background(), host, p.App, approval(&p, preview), rt); err != nil {
		t.Fatal(err)
	}
}

func TestPlanEffectiveReplicaBudget(t *testing.T) {
	m, host, p, rt := planFixture(t)
	// Include effective services that the plan itself does not select.
	for i := 1; i < 32; i++ {
		rt.Services[fmt.Sprintf("worker-%d", i)] = PlanService{Replicas: 1, Incarnation: fmt.Sprintf("%032x", i)}
	}
	previewPlan(t, m, host, p, rt)
	worker := rt.Services["worker-1"]
	worker.Replicas++
	rt.Services["worker-1"] = worker
	if _, err := m.ExecutePlan(context.Background(), host, p.App, PlanRequest{Action: "preview", Plan: &p}, rt); !errors.Is(err, errPlanSelection) {
		t.Fatalf("over-budget runtime: %v", err)
	}
}
