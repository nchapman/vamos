package cli

import (
	"bytes"
	"context"
	"github.com/nchapman/soar/internal/deploy"
	"strings"
	"testing"
)

func TestAppStatusShowsPendingOperationAndCandidate(t *testing.T) {
	_, _, deps := appFixture(t)
	deps.Run = func(_ context.Context, _ string, _ deploy.Request, _, _ []byte) (deploy.Response, error) {
		return deploy.Response{Status: &deploy.Status{Phase: "updating", Operation: "pending-operation", Services: map[string]deploy.ServiceStatus{"backend": {Kind: "private", Phase: "starting", Operation: "old-operation", Candidate: "replacement-container"}}}}, nil
	}
	var out, diagnostic bytes.Buffer
	if code := appCommand(context.Background(), "status", []string{"--server", "home", "--app", "hello"}, &out, &diagnostic, deps); code != 0 {
		t.Fatal(diagnostic.String())
	}
	if !strings.Contains(out.String(), "pending-operation") || !strings.Contains(out.String(), "replacement-container") {
		t.Fatal("recovery identities missing", out.String())
	}
}

func TestAppScopeRefusal(t *testing.T) {
	for _, args := range [][]string{{"--service", "web"}, {"--service", "backend"}, {"--candidate=false"}} {
		var out bytes.Buffer
		if code := appCommand(context.Background(), "deploy", args, &out, &out, appDeps{}); code != 2 {
			t.Fatalf("%v: code=%d", args, code)
		}
	}
}

func TestAppPrivateReadSelectors(t *testing.T) {
	for _, action := range []string{"status", "logs"} {
		_, _, deps := appFixture(t)
		deps.Run = func(_ context.Context, _ string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
			if r.Service != "backend" || r.Candidate != (action == "logs") {
				t.Fatalf("request=%+v", r)
			}
			return deploy.Response{Status: &deploy.Status{Services: map[string]deploy.ServiceStatus{"backend": {Kind: "private", Phase: "ready", Running: true}}}, LogService: "backend", LogContainer: "worker", LogCandidate: true, Logs: "hello\n"}, nil
		}
		args := []string{"--server", "home", "--app", "hello", "--service", "backend"}
		if action == "logs" {
			args = append(args, "--candidate")
		}
		var out, diagnostic bytes.Buffer
		if code := appCommand(context.Background(), action, args, &out, &diagnostic, deps); code != 0 {
			t.Fatalf("%s: %s", action, &diagnostic)
		}
		if !strings.Contains(out.String(), "backend") || strings.Contains(out.String(), "https://hello.") {
			t.Fatal(out.String())
		}
	}
}
