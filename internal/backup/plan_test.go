package backup

import (
	"reflect"
	"strings"
	"testing"
)

const journalPlanYAML = `app: journal
sources:
  entries:
    type: sqlite
    service: web
    volume: data
    path: journal.sqlite
  uploads:
    type: files
    service: web
    volume: data
    path: uploads
sets:
  daily:
    sources: [entries, uploads]
    consistency: quiesced
    stop: [web]
    repository: offsite
  database-only:
    sources: [entries]
    consistency: online-per-source
    repository: offsite
`
const postgresPlanYAML = `app: rails-ledger
sources:
  primary:
    type: postgres
    service: database
    database: rails_ledger
    user: rails_ledger
    credentials:
      service: database
      env: POSTGRES_PASSWORD
    restore:
      profile: single-owner
sets:
  daily:
    sources: [primary]
    consistency: online-per-source
    repository: offsite
`

func mustPlan(t *testing.T, text string) Plan {
	t.Helper()
	p, e := DecodePlan(strings.NewReader(text))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestPlanExamplesAndComposition(t *testing.T) {
	mustPlan(t, journalPlanYAML)
	p := mustPlan(t, postgresPlanYAML)
	if p.Sources["primary"].Port != 5432 {
		t.Fatal("port default")
	}
	p.Sources["uploads"] = Source{Type: "files", Service: "web", Volume: "data", Path: "uploads"}
	p.Sets["daily"] = Set{Sources: []string{"primary", "uploads"}, Consistency: "quiesced", Stop: []string{"worker", "web"}, Repository: "offsite"}
	if e := ValidatePlan(p); e != nil {
		t.Fatal(e)
	}
	s := p.Sources["primary"]
	s.Credentials.Service = "web"
	p.Sources["primary"] = s
	if e := ValidatePlan(p); e != nil {
		t.Fatal(e)
	}
}
func TestPlanNormalizationAndDigest(t *testing.T) {
	p := mustPlan(t, postgresPlanYAML)
	s := p.Sources["primary"]
	s.Port = 0
	p.Sources["primary"] = s
	p.Sets["daily"] = Set{Sources: []string{"primary"}, Consistency: "quiesced", Stop: []string{"worker", "web"}, Repository: "offsite"}
	n, e := NormalizePlan(p)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := PlanDigest(p)
	b, _ := PlanDigest(n)
	if a != b || len(a) != 64 {
		t.Fatal("noncanonical digest")
	}
	if p.Sources["primary"].Port != 0 || !reflect.DeepEqual(p.Sets["daily"].Stop, []string{"worker", "web"}) {
		t.Fatal("mutated input")
	}
	n.Sources["primary"].Credentials.Env = "OTHER"
	n.Sources["primary"].Restore.Profile = "other"
	n.Sets["daily"].Stop[0] = "other"
	if p.Sources["primary"].Credentials.Env != "POSTGRES_PASSWORD" || p.Sources["primary"].Restore.Profile != "single-owner" || p.Sets["daily"].Stop[0] != "worker" {
		t.Fatal("aliased input")
	}
	s = p.Sources["primary"]
	s.Database = "other"
	p.Sources["primary"] = s
	c, _ := PlanDigest(p)
	if a == c {
		t.Fatal("selection not hashed")
	}
}
func TestPlanOverlaps(t *testing.T) {
	for _, kind := range []string{"sqlite", "files"} {
		for _, otherKind := range []string{"sqlite", "files"} {
			for _, pair := range [][2]string{{"db", "db"}, {"db", "db/child"}, {"db/child", "db"}, {".", "db"}, {"db", "db-wal"}, {"db", "db-shm"}, {"db", "db-journal"}, {"db-wal/child", "db"}} {
				if pair[0] == "." && kind == "sqlite" {
					continue
				}
				t.Run(kind+"/"+otherKind+"/"+pair[0]+"/"+pair[1], func(t *testing.T) {
					p := mustPlan(t, journalPlanYAML)
					p.Sources["entries"] = Source{Type: kind, Service: "web", Volume: "data", Path: pair[0]}
					p.Sources["uploads"] = Source{Type: otherKind, Service: "web", Volume: "data", Path: pair[1]}
					delete(p.Sets, "database-only")
					want := sourcePathsOverlap(p.Sources["entries"], p.Sources["uploads"])
					if (ValidatePlan(p) != nil) != want {
						t.Fatal("overlap validation mismatch")
					}
				})
			}
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		for _, k := range []string{"files", "sqlite"} {
			a := Source{Type: "sqlite", Service: "web", Volume: "data", Path: "db"}
			b := Source{Type: k, Service: "web", Volume: "data", Path: "db" + suffix}
			if !sourcePathsOverlap(a, b) || !sourcePathsOverlap(b, a) {
				t.Fatal("missing reserved overlap")
			}
			b.Volume = "other"
			if sourcePathsOverlap(a, b) {
				t.Fatal("different volumes")
			}
			b.Volume = a.Volume
			b.Service = "other"
			if sourcePathsOverlap(a, b) {
				t.Fatal("different services")
			}
		}
	}
}
func TestPlanInvalidSelections(t *testing.T) {
	tests := map[string]func(*Plan){
		"pg same service": func(p *Plan) {
			p.Sources["second"] = p.Sources["primary"]
			s := p.Sources["second"]
			s.Database = "other"
			p.Sources["second"] = s
			set := p.Sets["daily"]
			set.Sources = append(set.Sources, "second")
			p.Sets["daily"] = set
		},
		"pg stopped": func(p *Plan) {
			s := p.Sets["daily"]
			s.Consistency = "quiesced"
			s.Stop = []string{"database"}
			p.Sets["daily"] = s
		},
		"duplicate stop": func(p *Plan) {
			s := p.Sets["daily"]
			s.Consistency = "quiesced"
			s.Stop = []string{"web", "web"}
			p.Sets["daily"] = s
		},
		"online stop": func(p *Plan) { s := p.Sets["daily"]; s.Stop = []string{"web"}; p.Sets["daily"] = s },
		"empty stop":  func(p *Plan) { s := p.Sets["daily"]; s.Consistency = "quiesced"; p.Sets["daily"] = s },
		"invalid stop": func(p *Plan) {
			s := p.Sets["daily"]
			s.Consistency = "quiesced"
			s.Stop = []string{"../web"}
			p.Sets["daily"] = s
		},
		"pg volume":         func(p *Plan) { s := p.Sources["primary"]; s.Volume = "data"; p.Sources["primary"] = s },
		"connection string": func(p *Plan) { s := p.Sources["primary"]; s.Database = "password=private"; p.Sources["primary"] = s },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			p := mustPlan(t, postgresPlanYAML)
			change(&p)
			if ValidatePlan(p) == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, v := range []string{"", ".", "/db", "../db", "a/../db", "a//db", "a/", `a\db`, "a/*", "${HOME}", "a\x00b", strings.Repeat("x/", 32) + "db", strings.Repeat("x", 513)} {
		p := mustPlan(t, journalPlanYAML)
		s := p.Sources["entries"]
		s.Path = v
		p.Sources["entries"] = s
		if ValidatePlan(p) == nil {
			t.Errorf("accepted invalid path %q", v)
		}
	}
	p := mustPlan(t, journalPlanYAML)
	s := p.Sources["entries"]
	s.Database = "db"
	p.Sources["entries"] = s
	if ValidatePlan(p) == nil {
		t.Fatal("sqlite PG field")
	}
}
func TestPlanStrictYAML(t *testing.T) {
	for name, text := range map[string]string{
		"duplicate": journalPlanYAML + "app: private\n",
		"alias":     strings.Replace(journalPlanYAML, "app: journal", "app: &private journal\nextra: *private", 1),
		"null":      strings.Replace(journalPlanYAML, "path: uploads", "path: null", 1),
		"unknown":   journalPlanYAML + "private: secret\n",
		"trailing":  journalPlanYAML + "---\napp: private\n",
		"oversized": journalPlanYAML + strings.Repeat("#private\n", MaxPlanBytes),
		"tag":       strings.Replace(journalPlanYAML, "app: journal", "app: !private journal", 1),
		"merge":     journalPlanYAML + "<<: {}\n",
		"version":   journalPlanYAML + "version: 1\n",
		"syntax":    "app: [private",
	} {
		t.Run(name, func(t *testing.T) {
			_, e := DecodePlan(strings.NewReader(text))
			if e == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(e.Error(), "private") || strings.Contains(e.Error(), "secret") {
				t.Fatal("leaked input")
			}
		})
	}
}
