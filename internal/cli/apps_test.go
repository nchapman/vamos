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

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/registry"
)

func TestAppsSelectsRegisteredServerWithoutProjectSideEffects(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			dir, store, reg := initFixture(t)
			t.Chdir(dir)
			// A corrupt project binding and no config must be irrelevant to inventory.
			if err := os.Mkdir(".vamos", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(".vamos", "server.json"), []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, server := range []registry.Server{{Name: "home", Target: "root@home", Domain: "example.com"}, {Name: "other", Target: "root@other", Domain: "other.example.com"}} {
				if err := store.Put(server); err != nil {
					t.Fatal(err)
				}
			}
			want := "home"
			args := []string{"--json", "--executor", "custom", "--timeout", "2s"}
			if explicit {
				want = "other"
				args = append(args, "--server", want)
			}
			calls := 0
			deps := appDeps{Registry: reg,
				Inspect: func(ctx context.Context, target string) (*host.Report, error) {
					if target != "root@"+want {
						t.Fatal(target)
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("no timeout")
					}
					calls++
					return &host.Report{Root: true, Supported: true, Architecture: "arm64"}, nil
				},
				Artifact: func(path, arch string) ([]byte, error) {
					if path != "custom" || arch != "arm64" {
						t.Fatal(path, arch)
					}
					calls++
					return []byte("binary"), nil
				},
				Run: func(ctx context.Context, target string, r deploy.Request, binary, archive []byte) (deploy.Response, error) {
					calls++
					if r.Action != "apps" || r.App != "" || r.Config != nil || r.Host.Name != want || target != "root@"+want || string(binary) != "binary" || len(archive) != 0 {
						t.Fatalf("%+v", r)
					}
					if err := deploy.ValidateRequest(r); err != nil {
						t.Fatal(err)
					}
					return deploy.Response{Version: 1, Apps: &deploy.AppList{Entries: []deploy.AppEntry{}}}, nil
				},
			}
			var out, diag bytes.Buffer
			if code := appsCommand(context.Background(), args, &out, &diag, deps); code != 0 {
				t.Fatalf("%d %s", code, &diag)
			}
			var result deploy.AppList
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Entries == nil {
				t.Fatal(out.String(), err)
			}
			if calls != 3 {
				t.Fatal(calls)
			}
			data, _ := os.ReadFile(filepath.Join(".vamos", "server.json"))
			if string(data) != "untouched" {
				t.Fatal("changed binding")
			}
			entries, err := os.ReadDir(".vamos")
			if err != nil || len(entries) != 1 {
				t.Fatal("created metadata", err)
			}
		})
	}
}
func TestAppsErrorsAndTextOutput(t *testing.T) {
	for _, kind := range []string{"no-server", "missing-server", "invalid-host", "invalid-target", "inspect", "unsupported", "artifact", "run", "missing-response", "bad-timeout", "positional", "text", "empty-text"} {
		t.Run(kind, func(t *testing.T) {
			_, store, reg := initFixture(t)
			server := registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}
			if kind == "invalid-host" {
				server.Domain = "invalid domain"
			}
			if kind == "invalid-target" {
				server.Target = "-bad"
			}
			if kind != "no-server" {
				if err := store.Put(server); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{}
			if kind == "missing-server" {
				args = []string{"--server", "other"}
			}
			if kind == "bad-timeout" {
				args = []string{"--timeout", "0"}
			}
			if kind == "positional" {
				args = []string{"notes"}
			}
			deps := appDeps{Registry: reg,
				Inspect: func(context.Context, string) (*host.Report, error) {
					if kind == "inspect" {
						return nil, errors.New("inspect failed")
					}
					return &host.Report{Root: kind != "unsupported", Supported: true, Architecture: "amd64"}, nil
				},
				Artifact: func(string, string) ([]byte, error) {
					if kind == "artifact" {
						return nil, errors.New("artifact failed")
					}
					return []byte("binary"), nil
				},
				Run: func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
					if kind == "run" {
						return deploy.Response{}, errors.New("run failed")
					}
					if kind == "missing-response" {
						return deploy.Response{Version: 1}, nil
					}
					if kind == "empty-text" {
						return deploy.Response{Version: 1, Apps: &deploy.AppList{Entries: []deploy.AppEntry{}}}, nil
					}
					return deploy.Response{Version: 1, Apps: &deploy.AppList{Entries: []deploy.AppEntry{{App: "gone", Phase: "destroyed", Services: []string{"db"}, ServiceCount: 1, RetainedSecrets: true, VolumeRecords: 1, RetainedVolumes: true}}}}, nil
				},
			}
			var out, diag bytes.Buffer
			code := appsCommand(context.Background(), args, &out, &diag, deps)
			if kind == "text" || kind == "empty-text" {
				for _, text := range []string{"home", "root@home", "recorded inventory", "not live health"} {
					if !strings.Contains(out.String(), text) {
						t.Errorf("missing %q: %s", text, &out)
					}
				}
				if kind == "empty-text" {
					if code != 0 || !strings.Contains(out.String(), "no registered apps") {
						t.Fatal(code, &out, &diag)
					}
					return
				}
				if code != 0 || !strings.Contains(out.String(), "destroyed") || !strings.Contains(out.String(), "db (1)") {
					t.Fatal(code, &out, &diag)
				}
			} else if code == 0 {
				t.Fatal("accepted error case")
			}
		})
	}
}
