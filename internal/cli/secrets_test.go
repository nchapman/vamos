package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/secrets"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretStageBindingAndSafeFailure(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Archive = func(string, config.Build) ([]byte, error) { t.Fatal("secret built archive"); return nil, nil }
	var out, diagnostic bytes.Buffer
	var operation string
	deps.Run = func(_ context.Context, target string, r deploy.Request, b, v []byte) (deploy.Response, error) {
		if target != "root@home" || r.Config != nil || r.SecretName != "token" || !secrets.ValidVersion(r.Operation) || string(v) != " canary " {
			t.Fatal("invalid stage request")
		}
		if !strings.Contains(diagnostic.String(), r.Operation) {
			t.Fatal("operation not printed before dispatch")
		}
		operation = r.Operation
		return deploy.Response{}, errors.New(" canary ")
	}
	code := secretsCommand(context.Background(), []string{"stage", "-f", path, "--stdin", "--json", "token"}, strings.NewReader(" canary \n"), &out, &diagnostic, deps, readSecretInput)
	if code != 1 || out.Len() != 0 || strings.Contains(diagnostic.String(), "canary") || !strings.Contains(diagnostic.String(), "outcome unknown") || !strings.Contains(diagnostic.String(), operation) || !strings.Contains(diagnostic.String(), "vamos secrets list --server home --app hello") || strings.Contains(diagnostic.String(), "resume") {
		t.Fatalf("code=%d output=%s diagnostics=%s", code, &out, &diagnostic)
	}
	bound, err := readBinding(filepath.Dir(path))
	if err != nil || bound == nil || bound.Server != "home" {
		t.Fatal("missing binding", err)
	}
}

func TestSecretInputBoundary(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{"  value  \r\n", "  value  ", false}, {"value\n", "value", false}, {"a\n\n", "", true}, {"", "", true}, {"a\x00", "", true}, {strings.Repeat("x", 8193), "", true},
	} {
		v, e := readSecretInput(context.Background(), strings.NewReader(tc.in), io.Discard, true)
		if (e != nil) != tc.bad || (!tc.bad && string(v) != tc.want) {
			t.Fatalf("input result length=%d err=%v", len(v), e)
		}
	}
	if _, e := readSecretInput(context.Background(), strings.NewReader("must-not-read"), io.Discard, false); e == nil {
		t.Fatal("nonterminal accepted without --stdin")
	}
}

func TestSecretRefusalAndMissingAcknowledgment(t *testing.T) {
	for _, tc := range []struct {
		response deploy.Response
		want     string
	}{
		{deploy.Response{Version: 1, Error: "canary", ErrorCode: "secret_version_limit"}, "256"},
		{deploy.Response{Version: 1, Error: "canary", ErrorCode: "unknown"}, "outcome unknown"},
		{deploy.Response{Version: 2, Error: "canary", ErrorCode: "secret_version_limit"}, "outcome unknown"},
		{deploy.Response{Version: 1}, "outcome unknown"},
	} {
		path, _, deps := appFixture(t)
		deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			return tc.response, nil
		}
		var out, diag bytes.Buffer
		code := secretsCommand(context.Background(), []string{"stage", "-f", path, "--stdin", "--json", "token"}, strings.NewReader("value\n"), &out, &diag, deps, readSecretInput)
		if code != 1 || out.Len() != 0 || strings.Contains(diag.String(), "canary") || !strings.Contains(diag.String(), tc.want) || !strings.Contains(diag.String(), "Inspect:") {
			t.Fatalf("%d %s %s", code, &out, &diag)
		}
	}
}

func TestSecretStagePrefersAgentTransportWhenConfigured(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		t.Fatal("legacy Run called when Agent was available")
		return deploy.Response{}, nil
	}
	calledAgent := false
	deps.Agent = func(_ context.Context, target string, r deploy.Request, v []byte) (deploy.Response, error) {
		calledAgent = true
		if target != "root@home" || r.Action != "secrets-stage" || r.SecretName != "token" || string(v) != "secret-val" {
			t.Fatalf("unexpected agent call: target=%q req=%+v val=%q", target, r, v)
		}
		return deploy.Response{
			Version: 1,
			Secret:  &secrets.Entry{Name: "token", Version: r.Operation, Latest: true},
		}, nil
	}
	var out, diag bytes.Buffer
	code := secretsCommand(context.Background(), []string{"stage", "-f", path, "--stdin", "--json", "token"}, strings.NewReader("secret-val\n"), &out, &diag, deps, readSecretInput)
	if code != 0 || !calledAgent {
		t.Fatalf("code=%d calledAgent=%v out=%s diag=%s", code, calledAgent, &out, &diag)
	}
}
