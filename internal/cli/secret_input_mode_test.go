package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/agentclient"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/secrets"
	"github.com/nchapman/vamos/internal/transport"
)

func TestSecretAdmissionBoundary(t *testing.T) {
	for _, action := range []string{"set", "stage"} {
		for _, refused := range []bool{false, true} {
			path, _, deps := appFixture(t)
			deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
				e := errors.New("connect to diagnostic-canary")
				if refused {
					e = transport.NotSubmitted(e)
				}
				return deploy.Response{}, e
			}
			var out, diag bytes.Buffer
			code := secretsCommand(context.Background(), []string{action, "-f", path, "--stdin", "--json", "token"}, strings.NewReader("input-canary"), &out, &diag, deps, readSecretInput)
			if code == 0 || out.Len() != 0 || strings.Contains(diag.String(), "canary") || strings.Contains(diag.String(), "outcome unknown") == refused || strings.Contains(diag.String(), "not submitted") != refused {
				t.Fatal("incorrect admission diagnostic", diag.String())
			}
		}
	}
}

func TestCredentialCommandsWithLostAccessDoNotRecommendForbiddenReads(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
		return deploy.Response{}, &agentclient.RemoteError{Code: "forbidden", Message: "private-canary"}
	}
	var out, diag bytes.Buffer
	code := secretsCommand(context.Background(), []string{"set", "-f", path, "--stdin", "--json", "token"}, strings.NewReader("input-canary"), &out, &diag, deps, readSecretInput)
	if code != 1 || !strings.Contains(diag.String(), "SSH key") || strings.Contains(diag.String(), "vamos resume") || strings.Contains(diag.String(), "canary") {
		t.Error(diag.String())
	}
	diag.Reset()
	code = backupCommandInteractive(context.Background(), []string{"repository", "list", "--json"}, strings.NewReader(""), &out, &diag, deps, readSecretInput, false)
	if code != 1 || !strings.Contains(diag.String(), "SSH key") || strings.Contains(diag.String(), "vamos backup repository show") || strings.Contains(diag.String(), "canary") {
		t.Error(diag.String())
	}
}

func TestSecretRecordedFailure(t *testing.T) {
	for _, action := range []string{"stage", "set"} {
		path, _, deps := appFixture(t)
		deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
			return deploy.Response{}, errors.Join(transport.ErrOperationFailed, errors.New("diagnostic-canary"))
		}
		var out, diag bytes.Buffer
		code := secretsCommand(context.Background(), []string{action, "-f", path, "--stdin", "--json", "token"}, strings.NewReader("input-canary"), &out, &diag, deps, readSecretInput)
		if code == 0 || out.Len() != 0 || !strings.Contains(diag.String(), "recorded a failure") || strings.Contains(diag.String(), "canary") || strings.Contains(diag.String(), "vamos resume") {
			t.Fatal(diag.String())
		}
	}
}

func TestSecretSetTerminalBeforePrompt(t *testing.T) {
	path, _, deps := appFixture(t)
	out, diag := newHelpAppsTerminal(120, true), newHelpAppsTerminal(120, true)
	read := func(context.Context, io.Reader, io.Writer, bool) ([]byte, error) {
		metadataContains(t, diag.String(), "SECRETS SET", "Store new values", "replace deployed consumers", "interrupt", "not atomic")
		return []byte("input-canary"), nil
	}
	deps.Agent = func(_ context.Context, _ string, r deploy.Request, _ []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, SetApplied: true, Secret: &secrets.Entry{Name: r.SecretName, Version: r.Operation}}, nil
	}
	code := secretsCommandInteractive(context.Background(), []string{"set", "-f", path, "--json", "token"}, helpUnreadInput{t}, out, diag, deps, read, true)
	if code != 0 || !json.Valid(out.Bytes()) || strings.Contains(diag.String()+out.String(), "canary") {
		t.Fatal(code, out.String(), diag.String())
	}
}

func TestSecretInputModeRefusal(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, names := range [][]string{{"one"}, {"one", "two"}} {
			path, _, deps := appFixture(t)
			args := []string{"set", "-f", path, "--json"}
			if terminal {
				args = append(args, "--stdin")
			}
			var out, diag bytes.Buffer
			code := secretsCommandInteractive(context.Background(), append(args, names...), helpUnreadInput{t}, &out, &diag, deps, nil, terminal)
			if code == 0 || out.Len() != 0 || !strings.Contains(diag.String(), "--stdin") || !strings.Contains(diag.String(), "terminal") {
				t.Fatalf("unexpected refusal: %d %q %q", code, out.String(), diag.String())
			}
		}
	}
}
