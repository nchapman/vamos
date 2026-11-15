package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/registry"
)

func TestBackupAddBehavior(t *testing.T) {
	for _, kind := range []string{"preview", "malformed", "oversized", "duplicate", "case-field", "terminal-stdin", "connect-no-id", "ssh", "wrong-config", "wrong-name", "wrong-operation", "wrong-id", "malformed-response", "unverified-response", "default-region", "success"} {
		t.Run(kind, func(t *testing.T) {
			dir, store, reg := initFixture(t)
			if err := store.Put(registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "kit.json")
			args := []string{"repository", "add", "--yes", "--mode", "new", "--endpoint", "https://s3.example.com", "--bucket", "archive", "--region", "us-east-1", "--recovery-file", path, "--credentials-stdin"}
			input := `{"access_key_id":"private-access","secret_access_key":"private-secret"}`
			switch kind {
			case "preview":
				args = append(args[:2], args[3:]...)
			case "default-region":
				args = append(args[:9], args[11:]...)
			case "malformed":
				input = `{`
			case "oversized":
				input = strings.Repeat("x", backup.MaxPrivateBytes+1)
			case "duplicate":
				input = `{"access_key_id":"first","access_key_id":"private-access","secret_access_key":"private-secret"}`
			case "case-field":
				input = `{"ACCESS_KEY_ID":"private-access","secret_access_key":"private-secret"}`
			case "connect-no-id", "wrong-id":
				args[4] = "connect"
				args = append(args, "--prefix", "existing")
				input = `{"access_key_id":"private-access","secret_access_key":"private-secret","password":"private-password"}`
				if kind == "wrong-id" {
					args = append(args, "--repository-id", strings.Repeat("b", 64))
				}
			}
			args = append(args, "archive")
			calls := 0
			var pending []byte
			var staged backup.RecoveryKit
			deps := appDeps{Registry: reg, Inspect: func(context.Context, string) (*host.Report, error) {
				calls++
				return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
			}, Artifact: func(string, string) ([]byte, error) { return nil, nil }, Run: func(_ context.Context, _ string, r deploy.Request, _, private []byte) (deploy.Response, error) {
				calls++
				var err error
				pending, err = os.ReadFile(path)
				if err != nil {
					t.Fatal("kit not durable before setup", err)
				}
				if err = json.Unmarshal(pending, &staged); err != nil {
					t.Fatal(err)
				}
				var creds map[string]string
				if json.Unmarshal(private, &creds) != nil || creds["password"] == "" {
					t.Fatal("missing private password")
				}
				public, _ := json.Marshal(r)
				for _, secret := range creds {
					if secret != "" && bytes.Contains(public, []byte(secret)) {
						t.Fatal("secret in public request")
					}
				}
				if staged.Operation != r.Backup.Operation || staged.Credentials.Password != creds["password"] {
					t.Fatal("kit differs from request")
				}
				if kind == "ssh" {
					return deploy.Response{}, errors.New("private-backend-diagnostic")
				}
				m := backup.Metadata{Name: r.Backup.Name, Operation: r.Backup.Operation, Config: *r.Backup.Config, Mode: r.Backup.Action, State: "ready", RepositoryID: strings.Repeat("a", 64), Format: 2}
				switch kind {
				case "unverified-response":
					m.State = "unverified"
				case "wrong-config":
					m.Config.Prefix = "other"
				case "wrong-name":
					m.Name = "other"
				case "wrong-operation":
					m.Operation = strings.Repeat("c", 32)
				case "malformed-response":
					m.RepositoryID = "bad"
				}
				return deploy.Response{Version: 1, Backup: &backup.Response{Repository: &m}}, nil
			}}
			var out, diag bytes.Buffer
			reader := strings.NewReader(input)
			code := backupCommandInteractive(context.Background(), args, reader, &out, &diag, deps, nil, kind == "terminal-stdin")
			success := kind == "success" || kind == "terminal-stdin" || kind == "default-region"
			if success {
				if code != 0 {
					t.Fatalf("%d %s", code, &diag)
				}
			} else if kind != "preview" && code == 0 {
				t.Fatal("accepted invalid input/response")
			}
			if kind == "preview" {
				if code != 0 || reader.Len() != len(input) {
					t.Fatal("preview read credentials")
				}
			}
			if len(pending) > 0 {
				after, _ := os.ReadFile(path)
				if !success && !bytes.Equal(after, pending) {
					t.Fatal("pending kit changed")
				}
				if success {
					var final backup.RecoveryKit
					json.Unmarshal(after, &final)
					if final.RepositoryID != strings.Repeat("a", 64) || final.Credentials != staged.Credentials {
						t.Fatal("identity/key not preserved")
					}
				}
			} else {
				if calls != 0 {
					t.Fatal("unexpected remote access")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("unexpected kit")
				}
			}
			for _, secret := range []string{"private-access", "private-secret", "private-password", "private-backend-diagnostic", staged.Credentials.Password} {
				if secret != "" && strings.Contains(out.String()+diag.String(), secret) {
					t.Fatal("secret diagnostic leak")
				}
			}
			if kind == "ssh" && (!strings.Contains(diag.String(), "outcome unknown") || !strings.Contains(diag.String(), "show --server home archive") || !strings.Contains(diag.String(), staged.Operation)) {
				t.Fatal(diag.String())
			}
		})
	}
}

func TestBackupObservationRequests(t *testing.T) {
	for _, action := range []string{"list", "show", "verify", "resume"} {
		for _, responseKind := range []string{"ok", "wrong-name", "wrong-operation", "refusal", "unknown-code", "raw-error"} {
			t.Run(action+"/"+responseKind, func(t *testing.T) {
				_, store, reg := initFixture(t)
				if err := store.Put(registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}); err != nil {
					t.Fatal(err)
				}
				op := strings.Repeat("d", 32)
				args := []string{"repository", action, "--json", "--executor", "custom", "--timeout", "2s"}
				if action != "list" {
					args = append(args, "archive")
				}
				if action == "resume" {
					args = append(args, op)
				}
				calls := 0
				deps := appDeps{Registry: reg, Inspect: func(ctx context.Context, target string) (*host.Report, error) {
					if _, ok := ctx.Deadline(); !ok || target != "root@home" {
						t.Fatal("target/deadline")
					}
					return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
				}, Artifact: func(path, arch string) ([]byte, error) {
					if path != "custom" || arch != "amd64" {
						t.Fatal(path, arch)
					}
					return []byte("artifact"), nil
				}, Run: func(_ context.Context, _ string, r deploy.Request, b, private []byte) (deploy.Response, error) {
					calls++
					want := action
					if action == "show" {
						want = "status"
					}
					if r.Action != "backup-repository" || r.Backup.Action != want || len(private) != 0 || string(b) != "artifact" || backup.ValidateRequest(*r.Backup) != nil {
						t.Fatalf("bad request %+v", r)
					}
					if action == "resume" && r.Backup.Operation != op {
						t.Fatal("changed original operation")
					}
					response := deploy.Response{Version: 1}
					switch responseKind {
					case "refusal":
						response.ErrorCode = "backup_missing"
						response.Error = "private-raw"
						return response, errors.New("private-raw")
					case "unknown-code":
						response.ErrorCode = "private-raw"
						return response, nil
					case "raw-error":
						response.Error = "private-raw"
						return response, nil
					}
					m := backup.Metadata{Name: "archive", Operation: op, Mode: "create", State: "ready", RepositoryID: strings.Repeat("a", 64), Format: 2}
					m.Config.Endpoint = "https://s3.example.com"
					m.Config.Bucket = "archive"
					m.Config.Region = "us-east-1"
					m.Config.Prefix = "existing"
					m.Config.Lookup = "path"
					if responseKind == "wrong-name" {
						m.Name = "other"
					}
					if responseKind == "wrong-operation" {
						m.Operation = strings.Repeat("e", 32)
					}
					response.Backup = &backup.Response{Repository: &m}
					if action == "list" {
						response.Backup = &backup.Response{Repositories: []backup.Metadata{m}}
					}
					return response, nil
				}}
				var out, diag bytes.Buffer
				code := backupCommandInteractive(context.Background(), args, strings.NewReader("must-not-read"), &out, &diag, deps, nil, false)
				wantFailure := responseKind == "refusal" || responseKind == "unknown-code" || responseKind == "raw-error" || (responseKind == "wrong-name" && action != "list") || (responseKind == "wrong-operation" && action == "resume")
				if (code != 0) != wantFailure || calls != 1 {
					t.Fatalf("code=%d calls=%d %s", code, calls, &diag)
				}
				if strings.Contains(out.String()+diag.String(), "private-raw") {
					t.Fatal("raw diagnostic leaked")
				}
				if responseKind == "refusal" && !strings.Contains(diag.String(), backup.RefusalMessage("backup_missing")) {
					t.Fatal("lost local refusal")
				}
				if !wantFailure {
					var result backup.Response
					if json.Unmarshal(out.Bytes(), &result) != nil {
						t.Fatal("invalid JSON output")
					}
				}
			})
		}
	}
}
