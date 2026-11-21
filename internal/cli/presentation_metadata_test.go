package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/secrets"
	"github.com/nchapman/vamos/internal/ssh"
)

func TestSecretSuggestionKeepsArgumentBoundaries(t *testing.T) {
	out := newHelpAppsTerminal(80, false)
	writeSecretFailure(out, "stage", "stage; echo surprise", "root@host", "$(id)", "operation", "")
	if !strings.Contains(out.String(), "vamos secrets list --server 'stage; echo surprise' --app '$(id)'") {
		t.Fatal("suggestion did not quote complete arguments:", out.String())
	}
}

func metadataContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	text = ansi.Strip(text)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

func TestMetadataInspectUIDAndJSON(t *testing.T) {
	// Report.UID is uint32: negative values are not representable. Preserve its full range.
	for _, uid := range []uint32{0, 1000, ^uint32(0)} {
		report := &host.Report{UID: uid, Supported: true, OS: "ubuntu", Version: "24.04", Problems: []string{"observed problem"}}
		for _, asJSON := range []bool{false, true} {
			out := newHelpAppsTerminal(120, true)
			var diag bytes.Buffer
			args := []string{"home"}
			if asJSON {
				args = append([]string{"--json"}, args...)
			}
			code := inspectServer(context.Background(), args, out, &diag, func(context.Context, string) (*host.Report, error) { return report, nil })
			if code != 0 || diag.Len() != 0 {
				t.Fatalf("code=%d diagnostics=%s", code, &diag)
			}
			if asJSON {
				want, _ := json.Marshal(report)
				if out.String() != string(want)+"\n" {
					t.Fatalf("JSON changed: %q", out.String())
				}
			} else {
				metadataContains(t, out.String(), "SERVER INSPECT", "Read-only inspection", "sudo was not invoked", "observed problem", "UID", fmt.Sprint(uid))
			}
		}
	}
}

func TestMetadataStatusAndMutationLabels(t *testing.T) {
	for _, color := range []bool{false, true} {
		out := newHelpAppsTerminal(120, color)
		metadataContains(t, out.p.renderServerStatus("home", "root@home", &ssh.Status{}), "SERVER STATUS", "not an app health check", "unmanaged", "no")
		state := &lifecycle.State{ID: "installation-id", Phase: "ready", Step: "verify", LastError: "observed error"}
		metadataContains(t, out.p.renderServerStatus("home", "root@home", &ssh.Status{State: state, Active: true}), "ready", "verify", "yes", "Last error", "observed error")
		if err := writeServerMutationStart(out, "add", "home", "root@home", "example.test"); err != nil {
			t.Fatal(err)
		}
		if err := writeServerMutationResult(out, "home", state, "No app routes configured."); err != nil {
			t.Fatal(err)
		}
		metadataContains(t, out.String(), "SERVER ADD", "Root host mutation authorized", "example.test", "Host operation result", "installation-id", "No app routes configured.")
	}
	var plain bytes.Buffer
	state := &lifecycle.State{ID: "id", Phase: "ready"}
	_ = writeServerMutationStart(&plain, "add", "home", "root@home", "example.test")
	_ = writeServerMutationResult(&plain, "home", state, "done")
	if want := "home / root@home / example.test: add (root host mutation authorized)\n" + caddyPatchNotice + "\nhome: ready (installation id)\ndone\n"; plain.String() != want {
		t.Fatalf("plain changed: %q", plain.String())
	}
}

func TestMetadataSecretFailuresDoNotLeak(t *testing.T) {
	for _, action := range []string{"stage", "list"} {
		for _, refusal := range []bool{false, true} {
			for _, color := range []bool{false, true} {
				path, _, deps := appFixture(t)
				deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
					if refusal {
						return deploy.Response{Version: 1, Error: "remote-diagnostic-canary", ErrorCode: "secret_version_limit"}, nil
					}
					return deploy.Response{}, errors.New("remote-diagnostic-canary")
				}
				out, diag := newHelpAppsTerminal(120, color), newHelpAppsTerminal(120, color)
				args := []string{action, "-f", path, "--json"}
				if action == "stage" {
					args = append(args, "--stdin", "token")
				}
				code := secretsCommand(context.Background(), args, strings.NewReader("secret-value-canary\n"), out, diag, deps, readSecretInput)
				if code != 1 || out.Len() != 0 {
					t.Fatalf("code=%d stdout=%q", code, out.String())
				}
				for _, forbidden := range []string{"remote-diagnostic-canary", "secret-value-canary"} {
					if strings.Contains(diag.String(), forbidden) {
						t.Fatalf("leaked %s", forbidden)
					}
				}
				if action == "stage" {
					metadataContains(t, diag.String(), "Operation/version", "vamos secrets list --server home --app hello")
					if refusal {
						metadataContains(t, diag.String(), "Staging refused", "256")
					} else {
						metadataContains(t, diag.String(), "outcome unknown")
					}
				} else {
					metadataContains(t, diag.String(), "secret metadata request failed")
				}
			}
		}
	}
}

func TestMetadataSecretListContracts(t *testing.T) {
	entries := []secrets.Entry{{Name: "token", Version: "version-id", Latest: true}}
	for _, mode := range []string{"plain", "terminal", "json"} {
		path, _, deps := appFixture(t)
		response := deploy.Response{Version: 1, SecretVersions: entries}
		deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			return response, nil
		}
		var plain, diag bytes.Buffer
		terminal := newHelpAppsTerminal(120, true)
		args := []string{"list", "-f", path}
		if mode == "json" {
			args = append(args, "--json")
		}
		out := &plain
		if mode == "plain" {
			if code := secretsCommand(context.Background(), args, nil, out, &diag, deps, readSecretInput); code != 0 {
				t.Fatal(code, diag.String())
			}
			if plain.String() != "token version-id latest=true\n" {
				t.Fatal(plain.String())
			}
		} else {
			if code := secretsCommand(context.Background(), args, nil, terminal, &diag, deps, readSecretInput); code != 0 {
				t.Fatal(code, diag.String())
			}
			if mode == "json" {
				want, _ := json.Marshal(response)
				if terminal.String() != string(want)+"\n" {
					t.Fatalf("JSON changed: %q", terminal.String())
				}
			} else {
				metadataContains(t, terminal.String(), "Retained secret metadata only", "values are not displayed", "token", "version-id", "yes")
			}
		}
	}
}

func TestMetadataInitOutputParity(t *testing.T) {
	for _, binding := range []string{"unbound", "bound to home"} {
		var plain bytes.Buffer
		if err := writeInitSuccess(&plain, "hello", "vamos.yaml", binding); err != nil {
			t.Fatal(err)
		}
		want := "Created vamos.yaml; " + binding + ". Uses Dockerfile with context '.'; no Dockerfile created and no remote contacted.\n"
		if plain.String() != want {
			t.Fatalf("plain changed: %q", plain.String())
		}
		out := newHelpAppsTerminal(120, true)
		if err := writeInitSuccess(out, "hello", "vamos.yaml", binding); err != nil {
			t.Fatal(err)
		}
		metadataContains(t, out.String(), "INIT", "hello", "Local configuration created", "vamos.yaml", binding, "Uses Dockerfile with context '.'; no Dockerfile created and no remote contacted.")
	}
}

func TestMetadataSecretStageSuccess(t *testing.T) {
	path, _, deps := appFixture(t)
	var operation string
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _ []byte, value []byte) (deploy.Response, error) {
		if string(value) != "secret-value-canary" {
			t.Fatal("input changed")
		}
		operation = r.Operation
		return deploy.Response{Version: 1, Secret: &secrets.Entry{Name: r.SecretName, Version: r.Operation}}, nil
	}
	out, diag := newHelpAppsTerminal(120, true), newHelpAppsTerminal(120, true)
	code := secretsCommand(context.Background(), []string{"stage", "-f", path, "--stdin", "token"}, strings.NewReader("secret-value-canary\n"), out, diag, deps, readSecretInput)
	if code != 0 {
		t.Fatal(code, diag.String())
	}
	metadataContains(t, out.String(), "SECRETS STAGE", "Secret version staged; deploy to apply.", "token", operation)
	if strings.Contains(out.String()+diag.String(), "secret-value-canary") {
		t.Fatal("secret leaked")
	}
	metadataContains(t, out.p.renderSecretList("home", "root@home", "hello", nil), "No retained secret versions reported.")
}
