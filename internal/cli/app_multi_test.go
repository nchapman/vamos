package cli

import (
	"bytes"
	"context"
	"github.com/nchapman/vamos/internal/deploy"
	"os"
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

func TestMultiDeploymentCompletionExplainsPublicReadiness(t *testing.T) {
	for _, kind := range []string{"public", "mixed", "private"} {
		for _, action := range []string{"deploy", "resume", "status"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				path, _, deps := appFixture(t)
				services := map[string]deploy.ServiceStatus{}
				yaml := "app: hello\nservices:\n"
				if kind != "private" {
					services["web"] = deploy.ServiceStatus{Kind: "public", Phase: "ready", URL: "https://hello.example.com", Available: true}
					yaml += "  web:\n    build: {context: ., dockerfile: Dockerfile}\n    http: {port: 80, health: /healthz}\n"
				}
				if kind != "public" {
					services["backend"] = deploy.ServiceStatus{Kind: "private", Phase: "ready", Available: true}
					yaml += "  backend:\n    build: {context: ., dockerfile: Dockerfile}\n"
				}
				if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
					t.Fatal(err)
				}
				deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
					return deploy.Response{Status: &deploy.Status{Phase: "ready", Services: services}}, nil
				}
				args := []string{"-f", path}
				if action == "resume" {
					args = append(args, strings.Repeat("a", 32))
				}
				var out, diagnostic bytes.Buffer
				if code := appCommand(context.Background(), action, args, &out, &diagnostic, deps); code != 0 {
					t.Fatal(diagnostic.String())
				}
				hasWarning := strings.Contains(out.String(), "Public DNS and certificate readiness are not implied by deployment success.")
				if hasWarning != (action != "status" && kind != "private") {
					t.Fatal("incorrect public readiness feedback:", out.String())
				}
				privateNote := strings.Contains(out.String(), "Private-only app: no public route is configured.")
				if privateNote != (action != "status" && kind == "private") {
					t.Fatal("incorrect private completion feedback:", out.String())
				}
			})
		}
	}
}

func TestAppScopeRefusal(t *testing.T) {
	for _, args := range [][]string{{"--service", ""}, {"--candidate=false"}} {
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
		if action == "logs" {
			if out.String() != "hello\n" || !strings.Contains(diagnostic.String(), "backend") {
				t.Fatalf("stdout=%s stderr=%s", &out, &diagnostic)
			}
		} else if !strings.Contains(out.String(), "backend") || strings.Contains(out.String(), "https://hello.") {
			t.Fatal(out.String())
		}
	}
}
