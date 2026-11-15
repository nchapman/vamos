package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
)

func planBoundaryRequest(action string) deploy.Request {
	r := deploy.Request{Version: 1, Action: "backup-plan", Host: appRequest().Host, App: "notes", BackupPlan: &backup.PlanRequest{Action: action}}
	if action != "show" {
		r.BackupPlan.Plan = &backup.Plan{App: r.App, Sources: map[string]backup.Source{"db": {Type: "sqlite", Service: "web", Volume: "data", Path: "db.sqlite"}}, Sets: map[string]backup.Set{"daily": {Sources: []string{"db"}, Consistency: "online-per-source", Repository: "offsite"}}}
	}
	if action == "apply" {
		r.BackupPlan.ExpectedRevision = "none"
		r.BackupPlan.ExpectedDigest = strings.Repeat("a", 64)
	}
	return r
}

func TestBackupPlanTypedResponseAndWorker(t *testing.T) {
	for _, action := range []string{"preview", "apply", "show"} {
		t.Run(action, func(t *testing.T) {
			r := planBoundaryRequest(action)
			plan := &backup.PlanResponse{CurrentRevision: "none", Warnings: []string{"metadata only"}}
			body, err := json.Marshal(deploy.Response{Version: 1, BackupPlan: plan, Logs: "private-canary", Plan: &deploy.Plan{}, Status: &deploy.Status{App: "private-canary"}, Backup: &backup.Response{}})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			got, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, func(_ context.Context, _ string, args []string, input []byte) (process.Result, error) {
				calls++
				script := appReadScript
				if action != "show" {
					script = appScript
				}
				command := "/bin/sh -c '" + strings.ReplaceAll(script, "'", "'\\''") + "'"
				if !strings.Contains(strings.Join(args, " "), command) {
					t.Fatal("wrong worker script")
				}
				return process.Result{Stdout: body, Stderr: []byte("private-canary")}, nil
			})
			if err != nil || calls != 1 || !reflect.DeepEqual(got, deploy.Response{Version: 1, BackupPlan: plan}) {
				t.Fatalf("unsanitized typed response: %+v %v", got, err)
			}
		})
	}
}

func TestBackupPlanArchiveAndInputBoundary(t *testing.T) {
	r := planBoundaryRequest("preview")
	metadata, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := actionArchive(metadata, []byte("binary"), nil, r.Action)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(payload))
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[h.Name] {
			t.Fatal("duplicate entry", h.Name)
		}
		seen[h.Name] = true
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		switch h.Name {
		case "request.json":
			if !bytes.Equal(b, metadata) || h.Mode != 0600 || len(b) > 64*1024 {
				t.Fatal("unsafe request metadata")
			}
		case "executor":
			if string(b) != "binary" || h.Mode != 0700 {
				t.Fatal("wrong executor")
			}
		case "checksum":
			if h.Mode != 0600 {
				t.Fatal("unsafe checksum")
			}
		default:
			t.Fatal("unexpected sidecar", h.Name)
		}
	}
	if !reflect.DeepEqual(seen, map[string]bool{"request.json": true, "executor": true, "checksum": true}) {
		t.Fatal(seen)
	}
	for _, input := range [][]byte{[]byte("source archive"), privateRepositoryInput(), []byte("secret value")} {
		if _, err := actionArchive(metadata, []byte("binary"), input, r.Action); err == nil {
			t.Fatal("archive accepted private input")
		}
		_, err := runApp(context.Background(), "root@home", r, []byte("binary"), input, func(context.Context, string, []string, []byte) (process.Result, error) {
			t.Fatal("input reached SSH")
			return process.Result{}, nil
		})
		if err == nil {
			t.Fatal("accepted input")
		}
	}
	r.BackupPlan.Plan.Sources["db"] = backup.Source{Type: "sqlite", Service: "web", Volume: "data", Path: strings.Repeat("a", backup.MaxPlanBytes+1)}
	if _, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("unbounded metadata reached SSH")
		return process.Result{}, nil
	}); err == nil {
		t.Fatal("accepted oversized plan")
	}
}

func TestBackupPlanFailuresNeverLeakAndGuideShow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		runErr     error
	}{
		{"transport", "private-canary", errors.New("private-canary")},
		{"malformed", `{"version":1,"logs":"private-canary"`, nil},
		{"trailing", `{"version":1,"backup_plan":{"current_revision":"none"}} private-canary`, nil},
		{"unknown", `{"version":1,"error":"private-canary","error_code":"private-canary"}`, errors.New("private-canary")},
		{"missing", `{"version":1,"logs":"private-canary"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runApp(context.Background(), "root@home", planBoundaryRequest("preview"), []byte("binary"), nil, func(context.Context, string, []string, []byte) (process.Result, error) {
				return process.Result{Stdout: []byte(tc.body), Stderr: []byte("private-canary")}, tc.runErr
			})
			if err == nil {
				t.Fatal("accepted unknown outcome")
			}
			if !reflect.DeepEqual(got, deploy.Response{}) || strings.Contains(err.Error(), "private-canary") {
				t.Fatalf("raw diagnostic escaped: %+v %v", got, err)
			}
			if !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "inspect recorded policy") {
				t.Fatalf("unknown outcome must guide recorded-policy inspection, not automatic retry: %v", err)
			}
		})
	}
}

func TestBackupPlanWhitelistedRefusals(t *testing.T) {
	for _, code := range []string{"backup_plan_conflict", "backup_plan_runtime"} {
		t.Run(code, func(t *testing.T) {
			message := deploy.RefusalMessage("backup-plan", code)
			if message == "" {
				t.Fatal("missing whitelist")
			}
			body, _ := json.Marshal(deploy.Response{Version: 1, Error: "private-canary", ErrorCode: code, Logs: "private-canary", BackupPlan: &backup.PlanResponse{}})
			got, err := runApp(context.Background(), "root@home", planBoundaryRequest("apply"), []byte("binary"), nil, func(context.Context, string, []string, []byte) (process.Result, error) {
				return process.Result{Stdout: body, Stderr: []byte("private-canary")}, errors.New("private-canary")
			})
			if err == nil || err.Error() != message || !reflect.DeepEqual(got, deploy.Response{Version: 1, Error: message, ErrorCode: code}) {
				t.Fatalf("incorrect refusal: %+v %v", got, err)
			}
		})
	}
}
