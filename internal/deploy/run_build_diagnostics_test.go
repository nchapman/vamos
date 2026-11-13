package deploy

import (
	"context"
	"errors"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
	"strings"
	"testing"
)

func TestRunBuildDiagnosticSurvivesSubmission(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	q := f.req
	q.Config = multiApp()
	q.Operation = strings.Repeat("2", 32)
	q.Action = "run"
	q.Service = "backend"
	q.Command = []string{"echo", "ok"}
	old := f.m.Run
	f.m.Run = func(ctx context.Context, n string, args []string, b []byte) (process.Result, error) {
		if args[0] != "buildx" {
			return old(ctx, n, args, b)
		}
		return process.Result{Stderr: []byte("Dockerfile: broken build")}, errors.New("failed")
	}
	_, err := f.m.executeRun(context.Background(), q, f.bundle(&config.App{Name: j.App, Services: map[string]config.Service{"backend": q.Config.Services["backend"]}}), j, &lifecycle.State{ID: j.HostID}, nil)
	if err == nil || !strings.Contains(err.Error(), "broken build") {
		t.Fatalf("lost diagnostic: %v", err)
	}
	status, err := f.m.runStatus(context.Background(), j, q.Operation)
	if err != nil || !strings.Contains(status.Error, "broken build") || status.ExitCode != nil {
		t.Fatalf("status: %+v %v", status, err)
	}
	logs, err := f.m.runLogs(context.Background(), j, q.Operation)
	if err != nil || !strings.Contains(logs, "broken build") {
		t.Fatalf("logs: %q %v", logs, err)
	}
}
