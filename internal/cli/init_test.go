package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/registry"
)

func initFixture(t *testing.T) (string, *registry.Store, func() (*registry.Store, error)) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := registry.New(t.TempDir())
	return dir, store, func() (*registry.Store, error) { return store, nil }
}
func TestInitGenerationAndBindings(t *testing.T) {
	for _, mode := range []string{"unbound", "default", "explicit", "private", "matching-binding"} {
		t.Run(mode, func(t *testing.T) {
			dir, store, reg := initFixture(t)
			home := registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}
			other := registry.Server{Name: "other", Target: "root@other", Domain: "other.example.com"}
			if mode != "unbound" {
				if err := store.Put(home); err != nil {
					t.Fatal(err)
				}
				if err := store.Put(other); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "matching-binding" {
				if err := bindProject(dir, "notes", home); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, ".vamos", ".gitignore")); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "vamos.yaml")
			args := []string{"notes", "--port", "8080", "-f", path}
			if mode == "explicit" {
				args = append(args, "--server", "other", "--service", "api", "--health", "/ready")
			}
			if mode == "private" {
				args = []string{"--private", "-f", path, "notes"}
			}
			var out, diagnostic bytes.Buffer
			if code := initCommand(args, &out, &diagnostic, reg); code != 0 {
				t.Fatalf("code=%d %s", code, &diagnostic)
			}
			app, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := deploy.ValidateApp(app); err != nil {
				t.Fatal(err)
			}
			name := "web"
			if mode == "explicit" {
				name = "api"
			}
			svc := app.Services[name]
			if app.Name != "notes" || len(app.Services) != 1 || svc.Build == nil || svc.Build.Context != "." || svc.Build.Dockerfile != "Dockerfile" {
				t.Fatalf("%+v", app)
			}
			if mode == "private" {
				if svc.HTTP != nil {
					t.Fatal("private HTTP")
				}
			} else {
				health := "/"
				if mode == "explicit" {
					health = "/ready"
				}
				if svc.HTTP == nil || svc.HTTP.Port != 8080 || svc.HTTP.Health != health {
					t.Fatalf("%+v", svc)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, "Dockerfile")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created Dockerfile")
			}
			bound, err := readBinding(dir)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unbound" {
				if bound != nil {
					t.Fatal("unexpected binding")
				}
				if _, err := os.Lstat(filepath.Join(dir, ".vamos")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unbound metadata side effect")
				}
			} else {
				wanted := "home"
				if mode == "explicit" {
					wanted = "other"
				}
				if bound == nil || bound.Server != wanted || bound.App != "notes" {
					t.Fatalf("%+v", bound)
				}
				ignore, err := os.ReadFile(filepath.Join(dir, ".vamos", ".gitignore"))
				if err != nil || string(ignore) != "*\n" {
					t.Fatal("unprotected metadata", err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if initCommand(args, &out, &diagnostic, reg) == 0 {
				t.Fatal("overwrote config")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("changed existing config")
			}
		})
	}
}
func TestInitFlagHandlingAndValidation(t *testing.T) {
	for _, args := range [][]string{{"notes", "--port", "80"}, {"--port", "80", "notes"}, {"--port=80", "notes", "--service=api"}, {"--service", "api", "notes", "--port", "80"}, {"--port", "80", "--", "notes"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir, _, reg := initFixture(t)
			var out, diag bytes.Buffer
			// Put the output option before --, if present.
			args = append([]string{"-f", filepath.Join(dir, "vamos.yaml")}, args...)
			if code := initCommand(args, &out, &diag, reg); code != 0 {
				t.Fatalf("%d %s", code, &diag)
			}
		})
	}
	for _, args := range [][]string{{"notes"}, {"notes", "--private", "--port", "80"}, {"notes", "--private", "--health", "/"}, {"notes", "--port", "0"}, {"notes", "--port", "65536"}, {"notes", "--port", "80", "--health", "bad"}, {"Bad", "--port", "80"}, {"notes", "--port", "80", "--service", "Bad"}, {"notes", "extra", "--port", "80"}, {"notes", "--port", "80", "--server", "missing"}, {"notes", "--port", "80", "--server="}, {"notes", "--port", "80", "--unknown"}, {"notes", "--port", "--private"}, {"notes", "--private", "--service", "--port"}} {
		t.Run("reject "+strings.Join(args, " "), func(t *testing.T) {
			dir, _, reg := initFixture(t)
			var out, diag bytes.Buffer
			path := filepath.Join(dir, "vamos.yaml")
			if code := initCommand(append([]string{"-f", path}, args...), &out, &diag, reg); code == 0 {
				t.Fatal("accepted invalid args")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("wrote invalid config")
			}
		})
	}
}
func TestInitUnsafeMetadataAndOutput(t *testing.T) {
	for _, kind := range []string{"existing", "output-symlink", "parent-symlink", "vamos-symlink", "binding-mismatch", "binding-symlink", "ignore-symlink", "ignore-content", "lock-symlink", "metadata-output"} {
		t.Run(kind, func(t *testing.T) {
			dir, store, reg := initFixture(t)
			home := registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}
			if err := store.Put(home); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "vamos.yaml")
			root := filepath.Join(dir, ".vamos")
			unrelated := filepath.Join(dir, "unrelated")
			if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			symlink := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Skip(err)
				}
			}
			switch kind {
			case "existing":
				if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "output-symlink":
				symlink(unrelated, path)
			case "parent-symlink":
				symlink(dir, filepath.Join(dir, "link"))
				path = filepath.Join(dir, "link", "vamos.yaml")
			case "vamos-symlink":
				symlink(t.TempDir(), root)
			case "binding-mismatch":
				if err := bindProject(dir, "other-app", home); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "binding-symlink":
					symlink(unrelated, filepath.Join(root, "server.json"))
				case "ignore-symlink":
					symlink(unrelated, filepath.Join(root, ".gitignore"))
				case "lock-symlink":
					symlink(unrelated, filepath.Join(root, "binding.lock"))
				case "ignore-content":
					if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("server.json\n"), 0600); err != nil {
						t.Fatal(err)
					}
				case "metadata-output":
					path = filepath.Join(root, "server.json")
				}
			}
			var out, diag bytes.Buffer
			if initCommand([]string{"notes", "--port", "80", "-f", path}, &out, &diag, reg) == 0 {
				t.Fatal("accepted unsafe filesystem")
			}
			if kind != "existing" && kind != "output-symlink" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("wrote config before validation", &diag)
				}
			}
			data, _ := os.ReadFile(unrelated)
			if string(data) != "keep" {
				t.Fatal("changed unrelated file")
			}
		})
	}
}
func TestInitRegistryFailureDoesNotWrite(t *testing.T) {
	dir, _, _ := initFixture(t)
	path := filepath.Join(dir, "vamos.yaml")
	var out, diag bytes.Buffer
	if initCommand([]string{"notes", "--port", "80", "-f", path}, &out, &diag, func() (*registry.Store, error) { return nil, errors.New("registry unavailable") }) == 0 {
		t.Fatal("ignored registry error")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created config")
	}
}

type initFailWriter struct{}

func (initFailWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }
func TestInitReportsCompletedWriteWhenOutputFails(t *testing.T) {
	dir, _, reg := initFixture(t)
	path := filepath.Join(dir, "vamos.yaml")
	var diag bytes.Buffer
	if initCommand([]string{"notes", "--private", "-f", path}, initFailWriter{}, &diag, reg) == 0 || !strings.Contains(diag.String(), "partially wrote") {
		t.Fatal(&diag)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatal("removed successful config", err)
	}
}
