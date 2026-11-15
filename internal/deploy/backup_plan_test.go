package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func boundaryPlanRequest(host lifecycle.Config, action string) Request {
	r := Request{Version: 1, Action: "backup-plan", App: "notes", Host: host, BackupPlan: &backup.PlanRequest{Action: action}}
	if action != "show" {
		r.BackupPlan.Plan = &backup.Plan{App: r.App, Sources: map[string]backup.Source{"db": {Type: "sqlite", Service: "web", Volume: "data", Path: "db.sqlite"}}, Sets: map[string]backup.Set{"daily": {Sources: []string{"db"}, Consistency: "online-per-source", Repository: "offsite"}}}
	}
	if action == "apply" {
		r.BackupPlan.ExpectedRevision = "none"
		r.BackupPlan.ExpectedDigest = strings.Repeat("a", 64)
	}
	return r
}

func TestBackupPlanRejectsEveryUnrelatedField(t *testing.T) {
	_, app := setup(t)
	base := boundaryPlanRequest(app.Host, "show")
	allowed := map[string]bool{"Version": true, "Action": true, "App": true, "Host": true, "BackupPlan": true}
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if allowed[field.Name] {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			r := base
			v := reflect.ValueOf(&r).Elem().Field(i)
			switch v.Kind() {
			case reflect.Bool:
				v.SetBool(true)
			case reflect.String:
				v.SetString("canary")
			case reflect.Int:
				v.SetInt(1)
			case reflect.Pointer:
				v.Set(reflect.New(v.Type().Elem()))
			case reflect.Map:
				v.Set(reflect.MakeMap(v.Type()))
			case reflect.Slice:
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			default:
				t.Fatalf("unhandled field %s", field.Name)
			}
			if err := ValidateRequest(r); err == nil {
				t.Fatalf("accepted unrelated %s", field.Name)
			}
		})
	}
	for _, action := range []string{"deploy", "resume", "status", "destroy", "apps", "logs", "run", "backup-repository", "secrets-stage", "secrets-list", "plan", "rollback"} {
		t.Run(action, func(t *testing.T) {
			r := base
			r.Action = action
			if ValidateRequest(r) == nil {
				t.Fatal("accepted BackupPlan on other action")
			}
		})
	}
	r := base
	r.Backup = repositoryRequest(app.Host).Backup
	if ValidateRequest(r) == nil {
		t.Fatal("combined repository and plan fields")
	}
}

func TestBackupPlanValidationAndJSON(t *testing.T) {
	_, app := setup(t)
	for _, action := range []string{"show", "preview", "apply"} {
		t.Run(action, func(t *testing.T) {
			r := boundaryPlanRequest(app.Host, action)
			if err := ValidateRequest(r); err != nil {
				t.Fatal(err)
			}
			if r.Mutating() != (action != "show") {
				t.Fatal("wrong mutation classification")
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var round Request
			if err = json.Unmarshal(b, &round); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r, round) {
				t.Fatalf("roundtrip changed schema: %s", b)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(b, &fields)
			if fields["backup_plan"] == nil || fields["plan"] != nil || fields["backup"] != nil {
				t.Fatalf("wrong schema: %s", b)
			}
			for _, bad := range []string{"", "../notes", "UPPER", "a/b"} {
				q := r
				q.App = bad
				if ValidateRequest(q) == nil {
					t.Errorf("accepted app %q", bad)
				}
			}
		})
	}
	for _, tc := range []struct {
		action, rev, digest string
		valid               bool
	}{
		{"apply", "none", strings.Repeat("a", 64), true}, {"apply", strings.Repeat("b", 64), strings.Repeat("c", 64), true},
		{"apply", "", strings.Repeat("a", 64), false}, {"apply", "none", "none", false}, {"apply", "none", strings.Repeat("A", 64), false}, {"apply", strings.Repeat("a", 63), strings.Repeat("a", 64), false},
		{"preview", "none", "", false}, {"preview", "", strings.Repeat("a", 64), false}, {"show", "none", "", false}, {"show", "", strings.Repeat("a", 64), false},
	} {
		r := boundaryPlanRequest(app.Host, tc.action)
		r.BackupPlan.ExpectedRevision = tc.rev
		r.BackupPlan.ExpectedDigest = tc.digest
		if (ValidateRequest(r) == nil) != tc.valid {
			t.Errorf("approval %+v", tc)
		}
	}
	r := boundaryPlanRequest(app.Host, "preview")
	r.BackupPlan.Plan.App = "other"
	if ValidateRequest(r) == nil {
		t.Fatal("accepted mismatched plan app")
	}
	r = boundaryPlanRequest(app.Host, "show")
	r.BackupPlan.Plan = boundaryPlanRequest(app.Host, "preview").BackupPlan.Plan
	if ValidateRequest(r) == nil {
		t.Fatal("show accepted proposed plan")
	}
}

func TestBackupPlanHostLockAndMetadataOnlyShow(t *testing.T) {
	m, app := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("metadata action invoked process")
		return process.Result{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path(lifecycle.LockPath)), 0755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(m.path(lifecycle.LockPath))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatal("lock", err)
	}
	defer lock.Close()
	for _, action := range []string{"preview", "apply"} {
		if _, err := m.Execute(context.Background(), boundaryPlanRequest(app.Host, action), nil); !errors.Is(err, errHostBusy) {
			t.Fatalf("%s bypassed lock: %v", action, err)
		}
	}
	r := boundaryPlanRequest(app.Host, "show")
	response, err := m.Execute(context.Background(), r, nil)
	if err != nil || response.BackupPlan == nil || response.BackupPlan.Current != nil || response.BackupPlan.CurrentRevision != "none" {
		t.Fatalf("nonexistent app show: %+v %v", response, err)
	}
	if _, err = os.Stat(m.dir(r.App)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("show created app", err)
	}
	for _, input := range [][]byte{[]byte("source archive"), repositoryCredentials()} {
		if _, err = m.Execute(context.Background(), r, input); err == nil {
			t.Fatal("accepted private input")
		}
	}
	r.Host.Name = "foreign"
	if _, err = m.Execute(context.Background(), r, nil); !errors.Is(err, errHostIdentity) {
		t.Fatal("show bypassed host ownership", err)
	}
}

func TestBackupPlanLegacyPreviewDoesNotMutate(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version >= 7 {
		t.Fatalf("fixture no longer legacy: %d", j.Version)
	}
	p := filepath.Join(f.m.dir(j.App), "state.json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	events := append([]string(nil), f.docker.events...)
	r := boundaryPlanRequest(f.req.Host, "preview")
	r.App = j.App
	r.BackupPlan.Plan.App = j.App
	response, err := f.m.Execute(context.Background(), r, nil)
	if err == nil || ErrorCode(err) != "backup_plan_legacy" || response.Error != RefusalMessage("backup-plan", "backup_plan_legacy") {
		t.Fatalf("unsafe runtime refusal: %+v %v", response, err)
	}
	after, err := os.ReadFile(p)
	if err != nil || string(before) != string(after) {
		t.Fatal("journal mutated", err)
	}
	if !reflect.DeepEqual(events, f.docker.events) {
		t.Fatal("runtime mutated")
	}
	if _, err = os.Stat(f.m.path("/var/lib/vamos/backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup root created", err)
	}
}

func TestBackupPlanObservationFailureDoesNotRecommendDeployment(t *testing.T) {
	for _, state := range []string{"runtime", "journal"} {
		t.Run(state, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			upgradeServiceJournal(j)
			if err := f.m.save(j); err != nil {
				t.Fatal(err)
			}
			want := "backup_plan_runtime"
			if state == "runtime" {
				for _, slot := range j.ServiceStates {
					delete(f.docker.containers, slot.Current.Container)
					break
				}
			} else {
				want = "backup_plan_app_state"
				if err := os.WriteFile(filepath.Join(f.m.dir(j.App), "state.json"), []byte("private-corrupt-journal"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := boundaryPlanRequest(f.req.Host, "preview")
			r.App = j.App
			r.BackupPlan.Plan.App = j.App
			response, err := f.m.Execute(context.Background(), r, nil)
			if err == nil || ErrorCode(err) != want || response.Error != RefusalMessage("backup-plan", want) {
				t.Fatalf("wrong refusal: %+v %v", response, err)
			}
			if strings.Contains(response.Error, "deployment") || strings.Contains(response.Error, "private-corrupt") {
				t.Fatalf("misleading or unsafe refusal: %s", response.Error)
			}
		})
	}
}

func TestBackupPlanShowRetainedPrivatePlanAfterDestroy(t *testing.T) {
	m, app := setup(t)
	host := strings.Repeat("a", 32)
	manager := backup.NewManager(m.Root, m.RuntimeDir)
	backend := &repositoryBackend{}
	manager.Backend = backend
	repo := repositoryRequest(app.Host)
	if _, err := manager.Execute(context.Background(), host, *repo.Backup, repositoryCredentials()); err != nil {
		t.Fatal(err)
	}
	r := boundaryPlanRequest(app.Host, "preview")
	runtime := backup.PlanRuntime{HostID: host, App: r.App, Services: map[string]backup.PlanService{"web": {Incarnation: strings.Repeat("1", 32), Replicas: 1, Volume: &backup.PlanVolume{ID: strings.Repeat("2", 32), Name: "data", Mount: "/data"}}}}
	preview, err := manager.ExecutePlan(context.Background(), host, r.App, *r.BackupPlan, runtime)
	if err != nil {
		t.Fatal(err)
	}
	approval := backup.PlanRequest{Action: "apply", Plan: r.BackupPlan.Plan, ExpectedRevision: preview.CurrentRevision, ExpectedDigest: preview.Digest}
	installed, err := manager.ExecutePlan(context.Background(), host, r.App, approval, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Current == nil {
		t.Fatal("missing installed record")
	}
	if _, err = os.Stat(m.path("/var/lib/vamos/backups/plans")); err != nil {
		t.Fatal("missing shared plan directory", err)
	}
	// Use the existing minimal completed-destruction receipt format. Show must
	// not need deployment configuration or inspect any former runtime.
	receipt := `{"version":1,"app":"notes","operation":"` + strings.Repeat("e", 32) + `","phase":"destroyed","HostID":"` + host + `"}`
	if err = atomic(filepath.Join(m.dir(r.App), "state.json"), []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("show inspected destroyed runtime")
		return process.Result{}, nil
	}
	calls := backend.calls
	response, err := m.Execute(context.Background(), boundaryPlanRequest(app.Host, "show"), nil)
	if err != nil || response.BackupPlan == nil || !reflect.DeepEqual(response.BackupPlan.Current, installed.Current) {
		t.Fatalf("retained plan: %+v %v", response, err)
	}
	if backend.calls != calls {
		t.Fatal("show contacted backend")
	}
	b, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-repository-credential") {
		t.Fatal("private repository credential leaked")
	}
	if err = os.RemoveAll(m.dir(r.App)); err != nil {
		t.Fatal(err)
	}
	response, err = m.Execute(context.Background(), boundaryPlanRequest(app.Host, "show"), nil)
	if err != nil || response.BackupPlan == nil || !reflect.DeepEqual(response.BackupPlan.Current, installed.Current) {
		t.Fatalf("plan depended on app directory: %+v %v", response, err)
	}
}
