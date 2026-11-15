package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/secrets"
)

func TestSecretBatchLiteralInput(t *testing.T) {
	v, err := readSecretBatch(context.Background(), strings.NewReader("b= 'x=$A' \r\na=a=b\n"), []string{"a", "b"})
	if err != nil || string(v["a"]) != "a=b" || string(v["b"]) != " 'x=$A' " {
		t.Fatalf("literal framing failed: %v", err)
	}
}
func TestSecretBatchRejectsInvalidInput(t *testing.T) {
	for _, in := range []string{"a=x\na=y\n", "a=x\n", "a=x\nc=y\n", "a=x\nb=\n", "a=x\nb=\xff\n", "a=x\nb=y\r\r\n", "a=x\nb=" + strings.Repeat("x", secrets.MaxValueBytes+1)} {
		if _, err := readSecretBatch(context.Background(), strings.NewReader(in), []string{"a", "b"}); err == nil {
			t.Fatal("accepted invalid batch")
		}
	}
}

func TestSecretBatchCommand(t *testing.T) {
	for _, action := range []string{"stage", "set"} {
		for _, interactive := range []bool{false, true} {
			path, _, deps := appFixture(t)
			var out, diag bytes.Buffer
			calls, reads := 0, 0
			deps.Archive = func(string, config.Build) ([]byte, error) { t.Fatal("archive called"); return nil, nil }
			deps.Run = func(_ context.Context, _ string, r deploy.Request, _, v []byte) (deploy.Response, error) {
				calls++
				if r.Action != "secrets-"+action || r.SecretName != "" || len(r.SecretNames) != 2 || r.Config != nil {
					t.Fatal("wrong request")
				}
				values, err := secrets.DecodeBatch(v, r.SecretNames)
				if err != nil || string(values["a"]) != "canary=a" || string(values["b"]) != "canary=b" {
					t.Fatal("wrong private payload")
				}
				entries := []secrets.Entry{}
				for _, n := range r.SecretNames {
					version := secrets.VersionFor(r.Operation, n)
					if !strings.Contains(diag.String(), version) || !strings.Contains(diag.String(), r.Operation) {
						t.Fatal("missing preflight identifiers")
					}
					entries = append(entries, secrets.Entry{Name: n, Version: version})
				}
				return deploy.Response{Version: 1, SecretVersions: entries, SetApplied: action == "set"}, nil
			}
			args := []string{action, "-f", path, "--json"}
			reader := readSecretInput
			if interactive {
				reader = func(_ context.Context, _ io.Reader, w io.Writer, _ bool) ([]byte, error) {
					n := []string{"a", "b"}[reads]
					reads++
					if !strings.Contains(diag.String(), "Secret "+n) {
						t.Fatal("missing separate prompt")
					}
					return []byte("canary=" + strings.ToLower(n)), nil
				}
			} else {
				args = append(args, "--stdin")
			}
			args = append(args, "a", "b")
			code := secretsCommand(context.Background(), args, strings.NewReader("a=canary=a\nb=canary=b\n"), &out, &diag, deps, reader)
			if code != 0 || calls != 1 || strings.Contains(out.String()+diag.String(), "canary") {
				t.Fatalf("code %d calls %d", code, calls)
			}
		}
	}
}

func TestSecretBatchInvalidNeverRuns(t *testing.T) {
	for _, args := range [][]string{{"a", "b"}, {"a=value", "b"}, {"a", "a"}} {
		path, _, deps := appFixture(t)
		deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			t.Fatal("invalid input dispatched")
			return deploy.Response{}, nil
		}
		full := append([]string{"set", "-f", path, "--stdin"}, args...)
		if secretsCommand(context.Background(), full, strings.NewReader("a=canary\nb=\n"), io.Discard, io.Discard, deps, readSecretInput) == 0 {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestSecretSetFailureIsValueFreeAndExplainsRecovery(t *testing.T) {
	for _, code := range []string{"", "secret_set_protected"} {
		t.Run(code, func(t *testing.T) {
			path, _, deps := appFixture(t)
			var out, diag bytes.Buffer
			operation := ""
			deps.Run = func(_ context.Context, _ string, r deploy.Request, _, input []byte) (deploy.Response, error) {
				operation = r.Operation
				return deploy.Response{Version: 1, Error: "remote-canary", ErrorCode: code, Logs: "remote-canary"}, nil
			}
			result := secretsCommand(context.Background(), []string{"set", "-f", path, "--stdin", "--json", "token"}, strings.NewReader("input-canary"), &out, &diag, deps, readSecretInput)
			if result != 1 || out.Len() != 0 || strings.Contains(diag.String(), "canary") || !strings.Contains(diag.String(), operation) || !strings.Contains(diag.String(), "only if status records this incomplete operation") {
				t.Fatal("unsafe or missing failure guidance", result, out.String(), diag.String())
			}
			if code != "" && !strings.Contains(diag.String(), "protected by update: manual") {
				t.Fatal("known refusal hidden")
			}
		})
	}
}

func TestSecretSetMissingAcknowledgementIsNotSuccess(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, SetApplied: true}, nil
	}
	if secretsCommand(context.Background(), []string{"set", "-f", path, "--stdin", "token"}, strings.NewReader("input-canary"), io.Discard, io.Discard, deps, readSecretInput) != 1 {
		t.Fatal("accepted incomplete acknowledgement")
	}
}

func TestSecretAckMismatch(t *testing.T) {
	r := deploy.Request{Action: "secrets-set", Operation: strings.Repeat("a", 32), SecretNames: []string{"a", "b"}}
	response := deploy.Response{SetApplied: true, SecretVersions: []secrets.Entry{{Name: "a", Version: secretVersion(r, "a")}, {Name: "b", Version: secretVersion(r, "b")}}}
	if !validSecretAck(r, response) {
		t.Fatal("valid ack rejected")
	}
	response.SecretVersions[1].Version = r.Operation
	if validSecretAck(r, response) {
		t.Fatal("mismatch accepted")
	}
}
