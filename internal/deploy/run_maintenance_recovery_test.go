package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

func TestRunPreflightResumesProvenPrestartRemoval(t *testing.T) {
	m, _ := setup(t)
	j := &journal{Version: 4, HostID: strings.Repeat("a", 32), Status: Status{App: "notes"}}
	op := strings.Repeat("b", 32)
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: op, Service: "web", Phase: "create-intent", Container: "vamos-run-" + op, Image: "sha256:" + strings.Repeat("c", 64)}, DockerID: strings.Repeat("d", 64), Config: config.Service{Image: "alpine:3", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	m.Run = func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
		if args[1] == "inspect" {
			return dockerJSON([]any{})
		}
		if args[1] == "ls" {
			return process.Result{}, nil
		}
		t.Fatalf("unexpected mutation %v", args)
		return process.Result{}, nil
	}
	if m.preflightRuns(context.Background(), j) == nil {
		t.Fatal("unproven missing prestart container accepted")
	}
	_, sum, err := resourceRead(runPath(m, j, op), runRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	j.Maintenance = &maintenance{Action: "cleanup", Operation: strings.Repeat("e", 32), Containers: []plannedContainer{{Name: r.Container, ID: r.DockerID, Operation: op}}, Files: []plannedFile{{Name: op + ".run.json", SHA256: sum}}}
	if err := m.preflightRuns(context.Background(), j); err != nil {
		t.Fatalf("recorded removal cannot resume: %v", err)
	}
	j.Maintenance.Files[0].SHA256 = strings.Repeat("f", 64)
	if m.preflightRuns(context.Background(), j) == nil {
		t.Fatal("accepted changed removal receipt")
	}
}
