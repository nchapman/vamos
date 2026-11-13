package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/process"
	"github.com/nchapman/soar/internal/secrets"
	"io"
	"strings"
	"testing"
)

func TestSecretWorkerAndSafeFailure(t *testing.T) {
	for _, action := range []string{"secrets-stage", "secrets-list"} {
		r := appRequest()
		r.Action = action
		r.Config = nil
		r.Operation = ""
		var input []byte
		if action == "secrets-stage" {
			r.Operation = strings.Repeat("b", 32)
			r.SecretName = "token"
			input = []byte("canary-secret")
		}
		called := false
		run := func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
			called = true
			command := strings.Join(args, " ")
			if strings.Contains(command, "canary-secret") {
				t.Fatal("argv leak")
			}
			if strings.Contains(command, "systemd-run") != (action == "secrets-stage") {
				t.Fatal("wrong worker")
			}
			return process.Result{Stdout: []byte("canary-secret"), Stderr: []byte("canary-secret")}, errors.New("canary-secret")
		}
		_, err := runApp(context.Background(), "root@home", r, []byte("binary"), input, run)
		if !called {
			t.Fatalf("not dispatched: %v", err)
		}
		if action == "secrets-stage" && (err == nil || strings.Contains(err.Error(), "canary-secret") || !strings.Contains(err.Error(), "unknown")) {
			t.Fatalf("unsafe failure: %v", err)
		}
	}
}

func TestSecretListAcceptsMetadataWithoutDeploymentStatus(t *testing.T) {
	for _, entries := range [][]secrets.Entry{nil, {{Name: "token", Version: strings.Repeat("b", 32), Latest: true}}} {
		r := appRequest()
		r.Action, r.Config, r.Operation = "secrets-list", nil, ""
		// readMulti returns just the version inventory, not a status object.
		body, err := json.Marshal(deploy.Response{Version: 1, SecretVersions: entries})
		if err != nil {
			t.Fatal(err)
		}
		for _, disconnected := range []bool{false, true} {
			response, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, func(context.Context, string, []string, []byte) (process.Result, error) {
				var transportErr error
				if disconnected {
					transportErr = errors.New("canary transport failure")
				}
				return process.Result{Stdout: body}, transportErr
			})
			if disconnected {
				if err == nil || strings.Contains(err.Error(), "canary") {
					t.Fatalf("unsafe transport failure: %v", err)
				}
				continue
			}
			if err != nil || response.Status != nil || len(response.SecretVersions) != len(entries) {
				t.Fatalf("valid metadata response rejected: %+v %v", response, err)
			}
		}
	}
}

func TestSecretActionArchive(t *testing.T) {
	value := []byte("canary-only-in-secret-file")
	payload, err := actionArchive([]byte(`{"action":"secrets-stage"}`), []byte("binary"), value, "secrets-stage")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(payload))
	seen := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "context.tar" {
			t.Fatal("context included")
		}
		if h.Name == "secret.value" {
			seen = true
			if h.Mode != 0600 || !bytes.Equal(data, value) {
				t.Fatal("invalid value entry")
			}
		} else if bytes.Contains(data, value) {
			t.Fatal("value leaked")
		}
	}
	if !seen {
		t.Fatal("no secret entry")
	}
	for _, action := range []string{"status", "logs", "resume", "cleanup", "destroy", "secrets-list"} {
		if _, err := actionArchive(nil, []byte("binary"), value, action); err == nil {
			t.Fatalf("%s accepted value", action)
		}
	}
	for _, value := range [][]byte{nil, []byte("a\nb"), bytes.Repeat([]byte("x"), 8193)} {
		if _, err := actionArchive(nil, []byte("binary"), value, "secrets-stage"); err == nil {
			t.Fatal("invalid value accepted")
		}
	}
}

func TestSecretRefusalEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		action, body string
		known        bool
	}{
		{"secrets-stage", `{"version":1,"error":"canary","error_code":"secret_version_limit"}`, true},
		{"secrets-list", `{"version":1,"error":"canary","error_code":"secret_store_unsafe"}`, true},
		{"secrets-list", `{"version":1,"error":"canary","error_code":"secret_version_limit"}`, false},
		{"secrets-stage", `{"version":1,"error":"canary","error_code":"unknown"}`, false},
		{"secrets-stage", `{"version":2,"error":"canary","error_code":"secret_version_limit"}`, false},
		{"secrets-stage", `{"version":1,"error":"canary","error_code":"secret_version_limit"} {}`, false},
		{"secrets-stage", `{"version":1,"error":"canary","error_code":"secret_version_limit"`, false},
		{"secrets-stage", ``, false},
	} {
		t.Run(tc.action+tc.body, func(t *testing.T) {
			r := appRequest()
			r.Action = tc.action
			r.Config = nil
			r.Operation = ""
			var input []byte
			if r.Action == "secrets-stage" {
				r.Operation = strings.Repeat("b", 32)
				r.SecretName = "token"
				input = []byte("value")
			}
			response, err := runApp(context.Background(), "root@home", r, []byte("binary"), input, func(context.Context, string, []string, []byte) (process.Result, error) {
				return process.Result{Stdout: []byte(tc.body), Stderr: []byte("canary")}, errors.New("canary")
			})
			if err == nil || strings.Contains(err.Error(), "canary") || strings.Contains(response.Error, "canary") {
				t.Fatalf("unsafe: %+v %v", response, err)
			}
			if (response.ErrorCode != "") != tc.known {
				t.Fatalf("classification: %+v", response)
			}
			if !tc.known && r.Action == "secrets-stage" && !strings.Contains(err.Error(), "unknown") {
				t.Fatal(err)
			}
		})
	}
}
