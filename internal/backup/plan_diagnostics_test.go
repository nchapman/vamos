package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPlanSelectionDiagnostics(t *testing.T) {
	sourceChange := func(name string, change func(*Source)) func(*Plan) {
		return func(p *Plan) { s := p.Sources[name]; change(&s); p.Sources[name] = s }
	}
	setChange := func(change func(*Set)) func(*Plan) {
		return func(p *Plan) { s := p.Sets["daily"]; change(&s); p.Sets["daily"] = s }
	}
	for _, tt := range []struct {
		name, base, want string
		change           func(*Plan)
	}{
		{"app name", journalPlanYAML, "app must be a lowercase name", func(p *Plan) { p.App = "PRIVATE_CANARY" }},
		{"source count", journalPlanYAML, "sources must contain 1–16 entries", func(p *Plan) { p.Sources = map[string]Source{} }},
		{"set count", journalPlanYAML, "sets must contain 1–8 entries", func(p *Plan) { p.Sets = map[string]Set{} }},
		{"source name", journalPlanYAML, "source labels and service references must be lowercase names", func(p *Plan) { p.Sources["PRIVATE_CANARY"] = p.Sources["entries"] }},
		{"source type", journalPlanYAML, "source type must be postgres, sqlite or files", sourceChange("entries", func(s *Source) { s.Type = "PRIVATE_CANARY" })},
		{"restore omitted", postgresPlanYAML, "PostgreSQL sources require restore.profile: single-owner", sourceChange("primary", func(s *Source) { s.Restore = nil })},
		{"restore unsupported", postgresPlanYAML, "PostgreSQL sources require restore.profile: single-owner", sourceChange("primary", func(s *Source) { s.Restore.Profile = "PRIVATE_CANARY" })},
		{"postgres volume", postgresPlanYAML, "PostgreSQL sources do not accept volume or path", sourceChange("primary", func(s *Source) { s.Volume = "private-canary" })},
		{"postgres identifiers", postgresPlanYAML, "PostgreSQL database and user must be 1–63 ASCII letters, digits, underscores or hyphens", sourceChange("primary", func(s *Source) { s.User = "PRIVATE_CANARY!" })},
		{"postgres port", postgresPlanYAML, "PostgreSQL port must be 0–65535; 0 defaults to 5432", sourceChange("primary", func(s *Source) { s.Port = 65536 })},
		{"credentials omitted", postgresPlanYAML, "PostgreSQL sources require credentials.service and credentials.env", sourceChange("primary", func(s *Source) { s.Credentials = nil })},
		{"credential env", postgresPlanYAML, "credentials.env must be an environment-variable name", sourceChange("primary", func(s *Source) { s.Credentials.Env = "private-canary" })},
		{"volume", journalPlanYAML, "SQLite/files sources require a lowercase volume name", sourceChange("entries", func(s *Source) { s.Volume = "PRIVATE_CANARY" })},
		{"path", journalPlanYAML, "source path must be a clean relative path", sourceChange("entries", func(s *Source) { s.Path = "/PRIVATE_CANARY" })},
		{"sqlite root", journalPlanYAML, "source path must be a clean relative path", sourceChange("entries", func(s *Source) { s.Path = "." })},
		{"sqlite postgres fields", journalPlanYAML, "SQLite/files sources do not accept database, user, port, credentials or restore", sourceChange("entries", func(s *Source) { s.Database = "private-canary" })},
		{"repository", journalPlanYAML, "set labels and repository references must be lowercase names", setChange(func(s *Set) { s.Repository = "PRIVATE_CANARY" })},
		{"empty selection", journalPlanYAML, "each set must select 1–16 sources", setChange(func(s *Set) { s.Sources = []string{} })},
		{"unknown source", journalPlanYAML, "set sources must reference declared source labels", setChange(func(s *Set) { s.Sources = []string{"private-canary"} })},
		{"duplicate selection", journalPlanYAML, "set sources must not contain duplicate references", setChange(func(s *Set) { s.Sources = []string{"entries", "entries"} })},
		{"consistency", journalPlanYAML, "consistency must be online-per-source or quiesced", setChange(func(s *Set) { s.Consistency = "PRIVATE_CANARY" })},
		{"empty stop", journalPlanYAML, "quiesced sets require an explicit nonempty stop list", setChange(func(s *Set) { s.Stop = nil })},
		{"stop owner", journalPlanYAML, "a quiesced set must include each selected SQLite/files owner in stop", setChange(func(s *Set) { s.Stop = []string{"private-canary"} })},
		{"duplicate stop", journalPlanYAML, "stop must contain unique lowercase service names", setChange(func(s *Set) { s.Stop = []string{"web", "web"} })},
		{"online stop", postgresPlanYAML, "online-per-source sets must not declare stop services", setChange(func(s *Set) { s.Stop = []string{"private-canary"} })},
		{"files online", journalPlanYAML, "files sources require quiesced consistency", setChange(func(s *Set) { s.Consistency = "online-per-source"; s.Stop = nil })},
		{"postgres stopped", postgresPlanYAML, "selected PostgreSQL services must remain running; stop application writers instead", setChange(func(s *Set) { s.Consistency = "quiesced"; s.Stop = []string{"database"} })},
		{"overlap", journalPlanYAML, "selected paths overlap within a set", sourceChange("uploads", func(s *Source) { s.Path = "journal.sqlite-wal" })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := mustPlan(t, tt.base)
			tt.change(&p)
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodePlan(bytes.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "private_canary") || strings.Contains(err.Error(), "private-canary") {
				t.Fatalf("input leaked: %v", err)
			}
		})
	}
}

func TestPlanDiagnosticsUseStableLabelOrder(t *testing.T) {
	for _, kind := range []string{"sources", "sets"} {
		t.Run(kind, func(t *testing.T) {
			p := mustPlan(t, postgresPlanYAML)
			want := "PostgreSQL sources require restore.profile: single-owner"
			if kind == "sources" {
				a := p.Sources["primary"]
				a.Restore = nil
				z := p.Sources["primary"]
				z.Type = "PRIVATE_CANARY"
				p.Sources = map[string]Source{"a": a, "z": z}
			} else {
				a := p.Sets["daily"]
				a.Consistency = "PRIVATE_CANARY"
				z := p.Sets["daily"]
				z.Repository = "PRIVATE_CANARY"
				p.Sets = map[string]Set{"a": a, "z": z}
				want = "consistency must be online-per-source or quiesced"
			}
			for i := 0; i < 100; i++ {
				err := ValidatePlan(p)
				if err == nil || err.Error() != want {
					t.Fatalf("unstable first diagnostic: want %q, got %v", want, err)
				}
			}
		})
	}
}

type privateErrorReader struct{}

func (privateErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("PRIVATE_CANARY reader failure")
}
func TestPlanReaderDiagnosticDoesNotExposeUnderlyingError(t *testing.T) {
	_, err := DecodePlan(privateErrorReader{})
	if err == nil || strings.Contains(err.Error(), "PRIVATE_CANARY") {
		t.Fatalf("unsafe read error: %v", err)
	}
}
