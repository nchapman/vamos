package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/backupbackend"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
)

func privateRepositoryInput() []byte {
	data, _ := json.Marshal(backupbackend.Credentials{AccessKeyID: "synthetic-access", SecretAccessKey: "repository-private-canary", Password: strings.Repeat("c", 64)})
	return data
}

func TestRepositoryCredentialSidecarIsPrivateAndNotMetadata(t *testing.T) {
	metadata := []byte(`{"version":1,"action":"backup-repository"}`)
	private := privateRepositoryInput()
	payload, err := actionArchive(metadata, []byte("binary"), private, "backup-repository")
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(payload))
	seen := false
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "repository.credentials" {
			seen = true
			if h.Mode != 0600 || !bytes.Equal(data, private) {
				t.Fatal("unsafe sidecar")
			}
		} else if bytes.Contains(data, []byte("repository-private-canary")) {
			t.Fatal("credentials leaked outside sidecar")
		}
		if h.Name == "context.tar" || h.Name == "secret.value" {
			t.Fatal("wrong payload domain")
		}
	}
	if !seen {
		t.Fatal("missing private sidecar")
	}
}

func TestRepositoryWorkerClassificationAndSanitizedFailures(t *testing.T) {
	for _, action := range []string{"create", "connect", "probe", "resume", "verify", "status", "list"} {
		t.Run(action, func(t *testing.T) {
			app := appRequest()
			r := deploy.Request{Version: 1, Action: "backup-repository", Host: app.Host, Backup: &backup.Request{Action: action, Name: "offsite"}}
			op := strings.Repeat("b", 32)
			var private []byte
			if r.Backup.NeedsCredentials() {
				r.Backup.Config = &backupbackend.Config{Endpoint: "https://storage.example", Bucket: "backups", Prefix: "vamos/" + op, Region: "us-east-1", Lookup: "path"}
				private = privateRepositoryInput()
				if action != "probe" {
					r.Backup.Operation = op
					r.Backup.Confirm = true
				}
				if action == "connect" {
					r.Backup.ExpectedID = strings.Repeat("a", 64)
				}
			}
			if action == "list" {
				r.Backup.Name = ""
			}
			if action == "resume" {
				r.Backup.Operation = op
			}
			called := false
			_, err := runApp(context.Background(), "root@home", r, []byte("binary"), private, func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
				called = true
				command := strings.Join(args, " ")
				if strings.Contains(command, "systemd-run") != r.Mutating() {
					t.Fatal("incorrect worker")
				}
				if strings.Contains(command, "repository-private-canary") {
					t.Fatal("secret in command")
				}
				return process.Result{Stdout: []byte("repository-private-canary"), Stderr: []byte("repository-private-canary")}, errors.New("repository-private-canary")
			})
			if !called || err == nil || strings.Contains(err.Error(), "repository-private-canary") || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("unsafe transport result: %v", err)
			}
		})
	}
}

func TestRepositoryFailureCodesUseOnlyLocalMessages(t *testing.T) {
	r := deploy.Request{Version: 1, Action: "backup-repository", Host: appRequest().Host, Backup: &backup.Request{Action: "status", Name: "offsite"}}
	for _, known := range []bool{false, true} {
		code := "private-diagnostic"
		if known {
			code = "backup_missing"
		}
		body, _ := json.Marshal(deploy.Response{Version: 1, Error: "private-diagnostic", ErrorCode: code})
		response, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, func(context.Context, string, []string, []byte) (process.Result, error) {
			return process.Result{Stdout: body}, errors.New("private-diagnostic")
		})
		if err == nil || strings.Contains(err.Error(), "private-diagnostic") || strings.Contains(response.Error, "private-diagnostic") {
			t.Fatal("raw diagnostic escaped")
		}
		if (response.ErrorCode != "") != known {
			t.Fatal("incorrect refusal classification")
		}
	}
}
