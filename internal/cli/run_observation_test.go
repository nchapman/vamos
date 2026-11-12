package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func TestRunMetadataDoesNotHideLogsOrAppStatus(t *testing.T) {
	for _, action := range []string{"status", "logs"} {
		t.Run(action, func(t *testing.T) {
			path, _, deps := appFixture(t)
			code := 7
			op := strings.Repeat("a", 32)
			deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
				return deploy.Response{Version: 1, Logs: "migration output\n", LogService: "web", Status: &deploy.Status{App: "hello", Phase: "ready", Run: &deploy.RunResult{Operation: op, Service: "web", Phase: "complete", ExitCode: &code}}}, nil
			}
			args := []string{"-f", path}
			if action == "logs" {
				args = append(args, "--run", op)
			}
			var out, stderr bytes.Buffer
			if got := appCommand(context.Background(), action, args, &out, &stderr, deps); got != 0 {
				t.Fatal(got, stderr.String())
			}
			if !strings.Contains(out.String(), "Run exit code: 7") {
				t.Fatal("missing command outcome", out.String())
			}
			want := `phase="ready"`
			if action == "logs" {
				want = "migration output"
			}
			if !strings.Contains(out.String(), want) {
				t.Fatal("run metadata hid requested output", out.String())
			}
		})
	}
}
