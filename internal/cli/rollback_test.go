package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/deploy"
)

func TestRollbackCapturedInputs(t *testing.T) {
	_, _, deps := appFixture(t)
	deps.Archive = nil // Any local build access is a bug.
	previous := ""
	for _, apply := range []bool{false, true} {
		deps.Run = func(ctx context.Context, target string, r deploy.Request, binary, archive []byte) (deploy.Response, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 24*time.Minute {
				t.Fatal("rollback needs mutation deadline")
			}
			if r.Action != "rollback" || r.App != "hello" || r.Apply != apply || !r.AllowDowntime || r.Service != "web" || len(r.Operation) != 32 || r.Operation == previous || r.Config != nil || r.ContextFormat != "" || len(archive) != 0 {
				t.Fatalf("request=%+v", r)
			}
			previous = r.Operation
			return deploy.Response{Version: 1, Status: &deploy.Status{App: "hello", Phase: "ready"}, RollbackPreview: &deploy.RollbackPreview{TargetOperation: "old", NeedsConfirmation: !apply, Services: []deploy.RollbackService{{Name: "web", CurrentImage: "current", TargetImage: "target", Downtime: true, SecretVersions: map[string]string{"TOKEN": "v1"}}, {Name: "worker", ExcludedReason: "no previous image"}}}}, nil
		}
		args := []string{"--app", "hello", "--server", "home", "-f", "/nonexistent/soar.yaml", "--service", "web", "--allow-downtime"}
		if apply {
			args = append(args, "--yes")
		}
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), "rollback", args, &out, &diagnostic, deps); code != 0 {
			t.Fatalf("code=%d: %s", code, &diagnostic)
		}
		for _, want := range []string{"old", "current -> target", "downtime=true", "TOKEN", "v1", "revoked", "worker", "no previous image"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, &out)
			}
		}
	}
}

func TestRollbackFlagMatrix(t *testing.T) {
	for _, args := range [][]string{{"--reuse-data"}, {"--supersede", "old"}, {"--candidate"}, {"--run", "old"}, {"--json"}, {"--service="}, {"extra"}, {"--app", "hello"}} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), "rollback", args, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("%v: %d %s", args, code, &out)
		}
	}
	for _, action := range []string{"resume", "deploy", "run", "status", "logs", "cleanup", "destroy"} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), action, []string{"--allow-downtime=false"}, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("%s: %d", action, code)
		}
	}
	for _, args := range [][]string{{"--yes", "existing"}, {"--service", "web", "existing"}, {"--allow-downtime", "existing"}} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), "resume", args, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("resume accepted new rollback flags %v: %d", args, code)
		}
	}
	var out bytes.Buffer
	if code := Run([]string{"rollback", "--help"}, &out, &out, "test"); code != 0 || !strings.Contains(out.String(), "--allow-downtime") {
		t.Fatalf("dispatch/help: %d %s", code, &out)
	}
}
