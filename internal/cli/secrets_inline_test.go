package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/secrets"
)

func TestSecretsInlineValuesUseSharedPrivateInput(t *testing.T) {
	for _, action := range []string{"stage", "set"} {
		for _, pairs := range [][]string{{"token=inline-canary=a=b"}, {"token=inline-canary=a=b", "other= 'inline-canary $literal' "}} {
			t.Run(action+strings.Join(pairs, "/"), func(t *testing.T) {
				path, _, deps := appFixture(t)
				var out, diag bytes.Buffer
				calls := 0
				deps.Run = func(_ context.Context, _ string, r deploy.Request, _, input []byte) (deploy.Response, error) {
					calls++
					if r.Action != "secrets-"+action || r.Config != nil {
						t.Fatal("wrong action/config")
					}
					data, _ := json.Marshal(r)
					if strings.Contains(string(data), "inline-canary") {
						t.Fatal("value escaped private payload")
					}
					values, err := deploy.SecretsInput(r, input)
					if err != nil {
						t.Fatal(err)
					}
					expected := map[string][]byte{}
					for _, pair := range pairs {
						name, value, _ := strings.Cut(pair, "=")
						expected[name] = []byte(value)
					}
					if !reflect.DeepEqual(values, expected) {
						t.Fatal("inline values were transformed")
					}
					response := deploy.Response{Version: 1, SetApplied: action == "set"}
					if r.SecretName != "" {
						response.Secret = &secrets.Entry{Name: r.SecretName, Version: r.Operation}
					} else {
						for _, name := range r.SecretNames {
							response.SecretVersions = append(response.SecretVersions, secrets.Entry{Name: name, Version: secrets.VersionFor(r.Operation, name)})
						}
					}
					return response, nil
				}
				args := append([]string{action, "-f", path, "--json"}, pairs...)
				read := func(context.Context, io.Reader, io.Writer, bool) ([]byte, error) {
					t.Fatal("inline input prompted")
					return nil, nil
				}
				code := secretsCommand(context.Background(), args, nil, &out, &diag, deps, read)
				if code != 0 || calls != 1 {
					t.Fatalf("code=%d calls=%d diagnostics=%s", code, calls, diag.String())
				}
				if strings.Contains(out.String()+diag.String(), "inline-canary") {
					t.Fatal("value echoed")
				}
			})
		}
	}
}

func TestSecretsInvalidInlineValuesNeverEchoOrDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"token="}, {"token=inline-canary", "token=other"}, {"token=inline-canary", "other"},
		{"--stdin", "token=inline-canary"}, {"--stdin=false", "token=inline-canary"},
		{"token=inline-canary\n"}, {"token=inline-canary\x00"}, {"token=inline-canary\xff"},
		{"token=" + strings.Repeat("inline-canary", 800)}, {"BAD=inline-canary"}, {"=inline-canary"},
		{"--timeout=inline-canary", "token=value"}, {"--unknown-inline-canary=value"},
	} {
		path, _, deps := appFixture(t)
		var out, diag bytes.Buffer
		deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			t.Fatal("invalid input dispatched")
			return deploy.Response{}, nil
		}
		full := append([]string{"set", "-f", path}, args...)
		if secretsCommand(context.Background(), full, nil, &out, &diag, deps, readSecretInput) == 0 {
			t.Fatal("accepted invalid input")
		}
		if strings.Contains(out.String()+diag.String(), "inline-canary") {
			t.Fatal("invalid value appeared in diagnostic")
		}
	}
}
