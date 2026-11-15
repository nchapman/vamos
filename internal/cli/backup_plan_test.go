package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupPlanHelpAliases(t *testing.T) {
	for _, action := range []string{"configure", "plan"} {
		args, ok := commandHelpArgs([]string{"backup", action})
		if !ok || strings.Join(args, " ") != "backup "+action+" --help" {
			t.Fatalf("help: %v %v", args, ok)
		}
		var out, err bytes.Buffer
		if backupPlanCommand(context.Background(), []string{action, "--help"}, nil, &out, &err, appDeps{}) != 0 || !strings.Contains(out.String(), "not implemented") {
			t.Fatal(out.String(), err.String())
		}
	}
}

func TestBackupPlanRejectInputBeforeDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.yaml")
	if err := os.WriteFile(path, []byte("app: secret-private-value\nunknown: secret-private-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"configure", "--app", "demo", "-f", path},
		{"configure", "-f", path},
		{"plan", "--app", "demo", "--yes"},
		{"plan", "demo", "--app", "demo"},
		{"configure", "--app", "demo", "-f", filepath.Dir(path)},
	} {
		var out, err bytes.Buffer
		if backupPlanCommand(context.Background(), args, nil, &out, &err, appDeps{}) == 0 {
			t.Fatalf("accepted %v", args)
		}
		if strings.Contains(out.String()+err.String(), "secret-private-value") {
			t.Fatal("input leaked")
		}
	}
}

func TestBackupPlanActionableLocalErrors(t *testing.T) {
	const valid = `app: private-canary
sources:
  primary:
    type: postgres
    service: database
    database: app_db
    user: app_user
    credentials: {service: database, env: POSTGRES_PASSWORD}
    restore: {profile: single-owner}
sets:
  app-state: {sources: [primary], consistency: online-per-source, repository: offsite}
`
	for _, tt := range []struct{ name, text, app, want string }{
		{"missing profile", strings.Replace(valid, "    restore: {profile: single-owner}\n", "", 1), "private-canary", "PostgreSQL sources require restore.profile: single-owner"},
		{"app mismatch", valid, "other", "backup plan app does not match --app"},
		{"unknown field", valid + "private-canary: PRIVATE_CANARY\n", "private-canary", "invalid backup plan fields"},
		{"syntax", "app: [PRIVATE_CANARY", "private-canary", "invalid backup plan YAML"},
		{"invalid selector", strings.Replace(valid, "POSTGRES_PASSWORD", "PRIVATE-CANARY", 1), "private-canary", "credentials.env must be an environment-variable name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "backup.yaml")
			if err := os.WriteFile(filename, []byte(tt.text), 0600); err != nil {
				t.Fatal(err)
			}
			var out, diagnostic bytes.Buffer
			// Empty dependencies deliberately make any registry/network access fail the test.
			code := backupPlanCommand(context.Background(), []string{"configure", "--app", tt.app, "-f", filename, "--yes"}, nil, &out, &diagnostic, appDeps{})
			if code != 2 || !strings.Contains(diagnostic.String(), tt.want) {
				t.Fatalf("code %d: %s", code, &diagnostic)
			}
			combined := strings.ToLower(out.String() + diagnostic.String())
			if strings.Contains(combined, "private-canary") || strings.Contains(combined, "private_canary") {
				t.Fatal("local input leaked")
			}
		})
	}
}

func TestBackupPlanPreviewAndCAS(t *testing.T) {
	for _, mode := range []string{"preview", "default-file", "apply", "apply-error", "wrong-target", "wrong-digest"} {
		t.Run(mode, func(t *testing.T) {
			_, _, deps := appFixture(t)
			filename := filepath.Join(t.TempDir(), "backup.yaml")
			text := `app: hello
sources:
  data: {type: sqlite, service: web, volume: data, path: db.sqlite}
sets:
  daily: {sources: [data], consistency: online-per-source, repository: archive}
`
			if err := os.WriteFile(filename, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := backup.DecodePlan(strings.NewReader(text))
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("a", 32)
			digest := strings.Repeat("b", 64)
			record := backup.PlanRecord{Version: 1, HostID: id, Revision: digest, Plan: p, UpdatedAt: "", Bindings: backup.PlanBindings{
				Sources: map[string]backup.SourceBinding{"data": {Service: "web", Incarnation: id, Volume: &backup.PlanVolume{ID: id, Name: "data", Mount: "/data"}}},
				Stops:   map[string]string{}, Repositories: map[string]backup.RepositoryPin{"archive": {ID: digest, ConfigDigest: digest}},
			}}
			digest, err = backup.PlanRevision(record)
			if err != nil {
				t.Fatal(err)
			}
			record.Revision = digest
			calls, inspects, artifacts := 0, 0, 0
			deps.Inspect = func(context.Context, string) (*host.Report, error) {
				inspects++
				return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
			}
			deps.Artifact = func(string, string) ([]byte, error) { artifacts++; return []byte("executor"), nil }
			deps.Run = func(_ context.Context, target string, r deploy.Request, b, a []byte) (deploy.Response, error) {
				calls++
				if target != "root@home" || a != nil || r.Action != "backup-plan" || r.App != "hello" || r.BackupPlan == nil {
					t.Fatalf("invalid transport: %+v", r)
				}
				if calls == 1 {
					if r.BackupPlan.Action != "preview" || r.BackupPlan.ExpectedRevision != "" || r.BackupPlan.ExpectedDigest != "" || !reflect.DeepEqual(r.BackupPlan.Plan, &p) {
						t.Fatalf("bad preview: %+v", r.BackupPlan)
					}
					// An apply must reuse captured bytes, not reopen the local file.
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
					if mode == "wrong-target" {
						record.Plan.App = "other"
					}
					if mode == "wrong-digest" {
						record.Revision = strings.Repeat("c", 64)
					}
					return deploy.Response{Version: 1, BackupPlan: &backup.PlanResponse{CurrentRevision: "none", Proposed: &record, Digest: digest}}, nil
				}
				if r.BackupPlan.Action != "apply" || r.BackupPlan.ExpectedRevision != "none" || r.BackupPlan.ExpectedDigest != digest || !reflect.DeepEqual(r.BackupPlan.Plan, &p) {
					t.Fatalf("bad CAS: %+v", r.BackupPlan)
				}
				if mode == "apply-error" {
					return deploy.Response{Version: 1, Error: "remote-secret"}, errors.New("ssh-secret")
				}
				installed := record
				installed.UpdatedAt = "2026-01-01T00:00:00Z"
				return deploy.Response{Version: 1, BackupPlan: &backup.PlanResponse{Applied: true, Current: &installed, CurrentRevision: digest}}, nil
			}
			args := []string{"configure", "--app", "hello", "-f", filename, "--json"}
			if mode == "default-file" {
				t.Chdir(filepath.Dir(filename))
				args = []string{"configure", "--app", "hello", "--json"}
			}
			if mode != "preview" && mode != "default-file" {
				args = append(args, "--yes")
			}
			var out, diagnostic bytes.Buffer
			code := backupPlanCommand(context.Background(), args, nil, &out, &diagnostic, deps)
			success := mode == "preview" || mode == "default-file" || mode == "apply"
			if (code == 0) != success {
				t.Fatalf("code %d: %s", code, &diagnostic)
			}
			want := 1
			if mode == "apply" || mode == "apply-error" {
				want = 2
			}
			if calls != want || inspects != want || artifacts != want {
				t.Fatalf("calls %d inspect %d artifact %d", calls, inspects, artifacts)
			}
			if strings.Contains(out.String()+diagnostic.String(), "secret") {
				t.Fatal("remote error leaked")
			}
			if mode == "apply-error" && !strings.Contains(diagnostic.String(), "vamos backup plan --server home --app hello") {
				t.Fatal("missing safe inspection guidance")
			}
			if success {
				var result backup.PlanResponse
				if json.Unmarshal(out.Bytes(), &result) != nil {
					t.Fatal("not typed JSON")
				}
			}
		})
	}
}

func TestBackupPlanShowEmpty(t *testing.T) {
	_, _, deps := appFixture(t)
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, archive []byte) (deploy.Response, error) {
		if archive != nil || r.App != "hello" || r.BackupPlan == nil || r.BackupPlan.Action != "show" || r.BackupPlan.Plan != nil {
			t.Fatalf("bad show: %+v", r)
		}
		return deploy.Response{Version: 1, BackupPlan: &backup.PlanResponse{CurrentRevision: "none"}}, nil
	}
	var out, diagnostic bytes.Buffer
	if backupPlanCommand(context.Background(), []string{"plan", "--app", "hello", "--json"}, nil, &out, &diagnostic, deps) != 0 {
		t.Fatal(&diagnostic)
	}
	var result backup.PlanResponse
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Current != nil || result.CurrentRevision != "none" {
		t.Fatal(&out)
	}
}
