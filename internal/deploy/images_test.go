package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/process"
)

func TestImageReferencesAndKeys(t *testing.T) {
	for _, ref := range []string{"postgres:17", "localhost:5000/team/db:v1", "registry.example/db@sha256:" + strings.Repeat("a", 64)} {
		if err := validImageReference(ref); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "--help", "postgres:17\n", "https://user:password@registry/db", "user:password@registry/db", "docker.io/UPPER", strings.Repeat("a", 1025)} {
		if validImageReference(ref) == nil {
			t.Errorf("accepted %q", ref)
		}
	}
	if imageKey(config.Service{Image: "postgres"}) != imageKey(config.Service{Image: "docker.io/library/postgres:latest"}) {
		t.Fatal("noncanonical key")
	}
	if len(imageKey(config.Service{Image: "postgres:17"})) != 32 {
		t.Fatal("key length")
	}
}

func TestImageVolumeAdmission(t *testing.T) {
	im := &inspection{}
	cfg := &config.Service{Volume: &config.Volume{Mount: "/var/lib/postgresql/data"}}
	if imageVolumesAllowed(im, nil, false) != nil {
		t.Fatal("empty volumes")
	}
	im.Config.Volumes = map[string]json.RawMessage{cfg.Volume.Mount: json.RawMessage(`{}`)}
	if err := imageVolumesAllowed(im, cfg, true); err != nil {
		t.Fatal(err)
	}
	if imageVolumesAllowed(im, cfg, false) == nil || imageVolumesAllowed(im, nil, true) == nil {
		t.Fatal("anonymous volume admitted")
	}
	im.Config.Volumes["/extra"] = json.RawMessage(`{}`)
	if imageVolumesAllowed(im, cfg, true) == nil {
		t.Fatal("extra volume admitted")
	}
	delete(im.Config.Volumes, "/extra")
	cfg.Volume.Mount += "/"
	if imageVolumesAllowed(im, cfg, true) == nil {
		t.Fatal("nonexact mount admitted")
	}
}

func TestResolvePrebuiltDurableIdentity(t *testing.T) {
	for _, retryMode := range []string{"ok", "missing", "drift"} {
		t.Run(retryMode, func(t *testing.T) {
			id := "sha256:" + strings.Repeat("a", 64)
			cfg := config.Service{Image: "postgres:17", Volume: &config.Volume{Mount: "/var/lib/postgresql/data"}}
			j := &journal{Status: Status{App: "example", Operation: strings.Repeat("b", 32)}}
			var calls [][]string
			retry := false
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := &Manager{Root: root}
			m.Run = func(_ context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name != "docker" || len(input) != 0 {
					t.Fatal("wrong external boundary")
				}
				calls = append(calls, append([]string(nil), args...))
				if reflect.DeepEqual(args, []string{"pull", "--quiet", cfg.Image}) {
					if retry {
						t.Fatal("retry pulled mutable tag")
					}
					return process.Result{}, nil
				}
				if !reflect.DeepEqual(args, []string{"image", "inspect", cfg.Image}) && !reflect.DeepEqual(args, []string{"image", "inspect", id}) {
					t.Fatalf("unexpected command %v", args)
				}
				if retry && args[2] != id {
					t.Fatal("retry inspected mutable tag")
				}
				if retry && retryMode == "missing" {
					return process.Result{}, errors.New("secret Docker output")
				}
				im := inspection{ID: id}
				im.Config.Volumes = map[string]json.RawMessage{cfg.Volume.Mount: json.RawMessage(`{}`)}
				if retry && retryMode == "drift" {
					im.ID = "sha256:" + strings.Repeat("c", 64)
				}
				b, _ := json.Marshal([]inspection{im})
				return process.Result{Stdout: b}, nil
			}
			got, err := m.resolvePrebuilt(context.Background(), j, cfg)
			if err != nil || got != id {
				t.Fatalf("resolve: %s %v", got, err)
			}
			if len(calls) != 3 {
				t.Fatalf("commands: %v", calls)
			}
			retry = true
			calls = nil
			got, err = m.resolvePrebuilt(context.Background(), j, cfg)
			if retryMode == "ok" {
				if err != nil || got != id {
					t.Fatal(got, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("missing sanitized refusal", err)
			}
			if len(calls) != 1 {
				t.Fatalf("retry commands: %v", calls)
			}
		})
	}
}
