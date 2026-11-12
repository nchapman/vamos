package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestMaintenanceRemovalIdentityBoundary(t *testing.T) {
	for _, mode := range []string{"replacement", "list-error", "missing", "running", "destroy", "network-replacement"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager()
			j := &journal{HostID: "host", Status: Status{App: "notes"}}
			op := strings.Repeat("a", 32)
			id := strings.Repeat("b", 64)
			image := "sha256:" + strings.Repeat("c", 64)
			name := "soar-notes-" + op
			p := plannedContainer{Name: name, ID: id, Image: image, Operation: op, NetworkID: strings.Repeat("e", 64)}
			w := &maintenance{Action: "cleanup", Containers: []plannedContainer{p}}
			if mode == "destroy" || mode == "network-replacement" {
				w.Action = "destroy"
			}
			if mode == "network-replacement" {
				w.Network = &plannedNetwork{Name: "soar-notes", ID: strings.Repeat("f", 64)}
			}
			mutations := []string{}
			inspects := 0
			m.Run = func(_ context.Context, n string, a []string, _ []byte) (process.Result, error) {
				if n != "docker" {
					t.Fatalf("unexpected %s", n)
				}
				if a[1] == "ls" {
					if mode == "list-error" {
						return process.Result{}, errors.New("daemon unavailable")
					}
					if mode == "missing" {
						return process.Result{}, nil
					}
					rid := id
					if mode == "replacement" {
						rid = strings.Repeat("d", 64)
					}
					b, _ := json.Marshal(resourceRow{ID: rid, Names: name})
					return process.Result{Stdout: b}, nil
				}
				if a[1] == "inspect" {
					if a[0] == "network" {
						v := resourceInspection{Name: "soar-notes"}
						v.ID = strings.Repeat("e", 64)
						v.Driver = "bridge"
						v.Labels = map[string]string{"soar.host": "host", "soar.app": "notes"}
						b, _ := json.Marshal([]resourceInspection{v})
						return process.Result{Stdout: b}, nil
					}
					inspects++
					v := resourceInspection{Name: "/" + name, Image: image}
					v.ID = id
					v.Config.Labels = map[string]string{"soar.host": "host", "soar.app": "notes", "soar.operation": op}
					v.HostConfig.NetworkMode = "soar-notes"
					v.NetworkSettings.Networks = map[string]json.RawMessage{"soar-notes": json.RawMessage(`{"NetworkID":"` + strings.Repeat("e", 64) + `"}`)}
					v.State.Running = mode == "running" || mode == "destroy"
					b, _ := json.Marshal([]resourceInspection{v})
					return process.Result{Stdout: b}, nil
				}
				mutations = append(mutations, strings.Join(a, " "))
				return process.Result{}, nil
			}
			e := m.removeMaintenanceResources(context.Background(), j, w)
			if mode == "missing" || mode == "destroy" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("unsafe removal accepted")
			}
			if mode == "destroy" {
				want := []string{"container update --restart=no " + id, "container stop " + id, "container rm " + id}
				if strings.Join(mutations, ";") != strings.Join(want, ";") || inspects != 3 {
					t.Fatalf("commands %v inspections %d", mutations, inspects)
				}
			} else if len(mutations) != 0 {
				t.Fatal(mutations)
			}
		})
	}
}

func TestMaintenanceFileReplayAndDrift(t *testing.T) {
	m := NewManager()
	m.Root = t.TempDir()
	j := &journal{Status: Status{App: "notes"}}
	dir := m.dir(j.App)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	name := strings.Repeat("a", 32) + ".context.tar"
	path := filepath.Join(dir, name)
	os.WriteFile(path, []byte("original"), 0600)
	_, hash, e := resourceRead(path, 32<<20)
	if e != nil {
		t.Fatal(e)
	}
	w := &maintenance{Action: "cleanup", Files: []plannedFile{{Name: name, SHA256: hash}}}
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("unexpected Docker call")
		return process.Result{}, nil
	}
	os.WriteFile(path, []byte("changed"), 0600)
	if m.removeMaintenanceResources(context.Background(), j, w) == nil {
		t.Fatal("removed changed file")
	}
	os.WriteFile(path, []byte("original"), 0600)
	for i := 0; i < 2; i++ {
		if e = m.removeMaintenanceResources(context.Background(), j, w); e != nil {
			t.Fatal(e)
		}
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if m.removeMaintenanceResources(context.Background(), j, w) == nil {
		t.Fatal("accepted symlink")
	}
}

func TestMaintenancePlanRejectsUnknownFiles(t *testing.T) {
	m := NewManager()
	m.Root = t.TempDir()
	j := &journal{Status: Status{App: "notes", Operation: strings.Repeat("a", 32)}}
	os.MkdirAll(m.dir(j.App), 0700)
	os.WriteFile(filepath.Join(m.dir(j.App), "surprise"), []byte("data"), 0600)
	m.Run = func(_ context.Context, _ string, a []string, _ []byte) (process.Result, error) {
		if a[1] != "ls" {
			t.Fatalf("unexpected command %v", a)
		}
		return process.Result{}, nil
	}
	if _, e := m.planMaintenance(context.Background(), j, "destroy", strings.Repeat("b", 32)); e == nil {
		t.Fatal("unknown file accepted")
	}
}

func TestMaintenanceInventoryIncludesUntaggedBuildOutputs(t *testing.T) {
	m := NewManager()
	id := "sha256:" + strings.Repeat("a", 64)
	m.Run = func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
		if args[1] == "ls" {
			if strings.Contains(strings.Join(args, " "), "--all") {
				return process.Result{Stdout: []byte(id + "\n")}, nil
			}
			return process.Result{}, nil
		}
		v := resourceInspection{}
		v.ID = id
		b, _ := json.Marshal([]resourceInspection{v})
		return process.Result{Stdout: b}, nil
	}
	images, err := m.resourceImages(context.Background())
	if err != nil || images[id] == nil {
		t.Fatalf("untagged output missing: %v %v", images, err)
	}
}

func TestMaintenanceImagesRetainNewTagsAndForeignReferences(t *testing.T) {
	for _, reason := range []string{"tag", "digest", "foreign-stopped", "foreign-index", "inventory-error"} {
		t.Run(reason, func(t *testing.T) {
			m := NewManager()
			m.Root = t.TempDir()
			j := &journal{Status: Status{App: "notes"}}
			os.MkdirAll(m.dir(j.App), 0700)
			id := "sha256:" + strings.Repeat("a", 64)
			cid := strings.Repeat("b", 64)
			m.Run = func(_ context.Context, _ string, a []string, _ []byte) (process.Result, error) {
				if a[1] == "rm" {
					t.Fatal("removed shared image")
				}
				if a[0] == "image" && a[1] == "ls" {
					if reason == "inventory-error" {
						return process.Result{}, errors.New("inventory unavailable")
					}
					return process.Result{Stdout: []byte(id + "\n")}, nil
				}
				if a[0] == "image" && a[1] == "inspect" {
					v := resourceInspection{}
					v.ID = id
					if reason == "tag" {
						v.RepoTags = []string{"external:latest"}
					}
					if reason == "digest" {
						v.RepoDigests = []string{"external@" + id}
					}
					b, _ := json.Marshal([]resourceInspection{v})
					return process.Result{Stdout: b}, nil
				}
				if a[0] == "container" && a[1] == "ls" {
					if reason != "foreign-stopped" && reason != "foreign-index" {
						return process.Result{}, nil
					}
					b, _ := json.Marshal(resourceRow{ID: cid, Names: "foreign"})
					return process.Result{Stdout: b}, nil
				}
				if a[0] == "container" && a[1] == "inspect" {
					v := resourceInspection{Image: id}
					if reason == "foreign-index" {
						v.Image = "sha256:" + strings.Repeat("c", 64)
						v.Config.Image = id
					}
					v.ID = cid
					b, _ := json.Marshal([]resourceInspection{v})
					return process.Result{Stdout: b}, nil
				}
				t.Fatalf("unexpected %v", a)
				return process.Result{}, nil
			}
			e := m.removeMaintenanceResources(context.Background(), j, &maintenance{Action: "destroy", Images: []string{id}})
			if reason == "inventory-error" {
				if e == nil {
					t.Fatal("inventory failure treated as absent")
				}
			} else if e != nil {
				t.Fatal(e)
			}
		})
	}
}
