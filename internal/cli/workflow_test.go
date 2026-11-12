package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/nchapman/soar/internal/deploy"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowInvalidOptions(t *testing.T) {
	for _, tc := range []struct {
		action string
		args   []string
	}{
		{"deploy", []string{"--yes"}}, {"run", []string{"--service", "web"}},
		{"run", []string{"--", "echo"}}, {"run", []string{"--service", "web", "--app", "hello", "--server", "home", "--", "echo"}},
		{"deploy", []string{"--run", "abc"}}, {"status", []string{"--run", "abc", "--service", "web"}},
		{"logs", []string{"--run", "abc", "--candidate=false"}}, {"status", []string{"--run="}},
	} {
		t.Run(tc.action+strings.Join(tc.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			if code := appCommand(context.Background(), tc.action, tc.args, &out, &out, appDeps{}); code != 2 {
				t.Fatalf("code=%d %s", code, &out)
			}
		})
	}
}

func TestRunExactArgvAndExit(t *testing.T) {
	path, _, deps := appFixture(t)
	var out, diagnostic bytes.Buffer
	command := []string{"./bin/migrate", "a b", "$HOME", "--flag"}
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, bundle []byte) (deploy.Response, error) {
		if !reflect.DeepEqual(r.Command, command) || r.Service != "web" || len(bundle) == 0 || !strings.Contains(out.String(), r.Operation) {
			t.Fatalf("request=%+v output=%s", r, &out)
		}
		code := 23
		return deploy.Response{Version: 1, Run: &deploy.RunResult{Operation: r.Operation, ExitCode: &code, Output: "100% done\n"}}, nil
	}
	args := append([]string{"-f", path, "--service", "web", "--"}, command...)
	if code := appCommand(context.Background(), "run", args, &out, &diagnostic, deps); code != 23 {
		t.Fatalf("code=%d %s", code, &diagnostic)
	}
	if !strings.Contains(out.String(), "100% done") {
		t.Fatal(&out)
	}
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{}, errors.New("disconnected")
	}
	out.Reset()
	diagnostic.Reset()
	if code := appCommand(context.Background(), "run", args, &out, &diagnostic, deps); code == 0 {
		t.Fatal("unknown run succeeded")
	}
	if strings.Contains(diagnostic.String(), "resume") || !strings.Contains(diagnostic.String(), "status --run") {
		t.Fatal(&diagnostic)
	}
}

func TestScopedDeploymentPreviewAndApproval(t *testing.T) {
	for _, approve := range []bool{false, true} {
		path, _, deps := appFixture(t)
		deps.Run = func(_ context.Context, _ string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
			if r.Service != "web" || r.Apply != approve {
				t.Fatalf("request=%+v", r)
			}
			response := deploy.Response{Version: 1, DeployPreview: &deploy.DeploymentPreview{Service: "web", CurrentImage: "old", DesiredImage: "new", Downtime: true, NeedsConfirmation: true}}
			if approve {
				response.Status = &deploy.Status{Services: map[string]deploy.ServiceStatus{"web": {Phase: "ready"}}}
			}
			return response, nil
		}
		args := []string{"-f", path, "--service", "web"}
		if approve {
			args = append(args, "--yes")
		}
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), "deploy", args, &out, &diagnostic, deps); code != 0 {
			t.Fatalf("code=%d %s", code, &diagnostic)
		}
		if !strings.Contains(out.String(), `current-image="old" desired-image="new" downtime=true`) || strings.Contains(out.String(), "Whole-app") || strings.Contains(diagnostic.String(), "resume") {
			t.Fatalf("out=%s err=%s", &out, &diagnostic)
		}
		if !approve && !strings.Contains(out.String(), "soar deploy --service web --yes") {
			t.Fatal(&out)
		}
	}
}
