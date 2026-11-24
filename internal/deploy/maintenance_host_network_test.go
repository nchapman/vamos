package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestMaintenanceCapturedHostNetwork(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical=%t", historical), func(t *testing.T) {
			f := newMultiMaintenanceInventory(t)
			ctx := context.Background()
			rel := multiMaintenanceRelease("worker", strings.Repeat("7", 32), "sha256:"+strings.Repeat("c", 64), false)
			rel.Config.NetworkMode = "host"
			v := f.add(rel)
			v.HostConfig.NetworkMode = "host"
			v.State.Running = true
			v.HostConfig.RestartPolicy.Name = "unless-stopped"
			delete(f.net.Containers, v.ID)
			host := &resourceInspection{Name: "host"}
			host.Driver = "host"
			host.ID = strings.Repeat("1", 64)
			v.NetworkSettings.Networks = map[string]json.RawMessage{"host": json.RawMessage(`{"NetworkID":"` + host.ID + `"}`)}
			run := f.m.Run
			f.m.Run = func(ctx context.Context, name string, args []string, in []byte) (process.Result, error) {
				if strings.Join(args, " ") == "network inspect host" {
					b, err := json.Marshal([]*resourceInspection{host})
					return process.Result{Stdout: b}, err
				}
				res, err := run(ctx, name, args, in)
				if args[0] == "container" && args[1] == "stop" {
					v.NetworkSettings.Networks["host"] = json.RawMessage(`{"NetworkID":""}`)
				}
				return res, err
			}
			op := strings.Repeat("e", 32)
			if _, err := f.m.planMaintenance(ctx, f.j, "destroy", op); err == nil {
				t.Fatal("accepted host labels without capture")
			}
			f.j.PreviousCompleted = &completedRelease{Operation: rel.Operation, Services: map[string]*release{"worker": rel}}
			cleanup, err := f.m.planMaintenance(ctx, f.j, "cleanup", op)
			if err != nil || len(cleanup.Containers) != 0 {
				t.Fatalf("cleanup: %v %v", cleanup, err)
			}
			// The worker is now older than both retained snapshots. An ordinary build
			// IID for this operation/image cannot authorize its host attachment.
			var receipt string
			if historical {
				f.j.PreviousCompleted = nil
				v.HostConfig.RestartPolicy.Name = "no"
				v.State.Running = false
				f.file(t, rel.Operation+"."+imageKey(*rel.Config)+".image.iid", rel.Image+"\n")
				f.file(t, filepath.Base(f.m.hostReceiptPath(f.j, "other", containerName(f.j.App, "other", rel.Operation), rel.Operation)), rel.Image+"\n")
				if _, err := f.m.planMaintenance(ctx, f.j, "cleanup", op); err == nil {
					t.Fatal("ordinary IID authorized host mode")
				}
				receipt = filepath.Base(f.m.hostReceiptPath(f.j, rel.Service, rel.Container, rel.Operation))
				f.file(t, receipt, rel.Image+"\n")
				cleanup, err = f.m.planMaintenance(ctx, f.j, "cleanup", op)
				if err != nil || len(cleanup.Containers) != 1 {
					t.Fatalf("historical cleanup: %v %v", cleanup, err)
				}
			}
			row := resourceRow{Name: rel.Container, ID: v.ID}
			for _, tc := range []struct {
				name  string
				alter func(*resourceInspection)
			}{
				{"image", func(c *resourceInspection) { c.Config.Image = "sha256:" + strings.Repeat("a", 64) }},
				{"mode", func(c *resourceInspection) { c.HostConfig.NetworkMode = network(f.j) }},
				{"publish", func(c *resourceInspection) { c.HostConfig.PublishAllPorts = true }},
				{"extra attachment", func(c *resourceInspection) {
					c.NetworkSettings.Networks = map[string]json.RawMessage{"host": json.RawMessage(`{}`), network(f.j): json.RawMessage(`{}`)}
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					changed := *v
					tc.alter(&changed)
					if f.m.resourceContainer(&changed, f.j, row) == nil {
						t.Fatal("accepted changed host workload")
					}
				})
			}
			w, err := f.m.planMaintenance(ctx, f.j, "destroy", op)
			if err != nil {
				t.Fatal(err)
			}
			if len(w.Containers) != 1 || w.Containers[0].NetworkID != host.ID || w.Network.ID != f.net.ID {
				t.Fatal("incorrect network capture")
			}
			if historical {
				if err := os.Remove(filepath.Join(f.m.dir(f.j.App), receipt)); err != nil {
					t.Fatal(err)
				}
				if err := f.m.removeMaintenanceResources(ctx, f.j, w); err == nil || len(f.mutations) != 0 {
					t.Fatal("acted without historical receipt")
				}
				f.file(t, receipt, "sha256:"+strings.Repeat("a", 64)+"\n")
				if err := f.m.removeMaintenanceResources(ctx, f.j, w); err == nil || len(f.mutations) != 0 {
					t.Fatal("acted on changed receipt")
				}
				f.file(t, receipt, rel.Image+"\n")
				v.State.Running = true
				if err := f.m.removeMaintenanceResources(ctx, f.j, w); err == nil || len(f.mutations) != 0 {
					t.Fatal("acted on running historical host workload")
				}
				v.State.Running = false
			}
			id := host.ID
			host.ID = strings.Repeat("2", 64)
			if err := f.m.removeMaintenanceResources(ctx, f.j, w); err == nil || len(f.mutations) != 0 {
				t.Fatal("acted on changed host network")
			}
			host.ID = id
			if err := f.m.removeMaintenanceResources(ctx, f.j, w); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHostNetworkCreateReceipt(t *testing.T) {
	m, j, r, f := multiDockerFixture(t, false)
	r.Config.NetworkMode = "host"
	j.Config.Services[r.Service] = *r.Config
	j.ServiceStates[r.Service].Phase = "prepared"
	path := m.hostReceiptPath(j, r.Service, r.Container, r.Operation)
	original := m.Run
	attempts := 0
	m.Run = func(ctx context.Context, name string, args []string, in []byte) (process.Result, error) {
		if name == "docker" && args[0] == "create" {
			attempts++
			id, err := readHostReceipt(path)
			if err != nil || id != r.Image {
				t.Fatalf("receipt before create: %q, %v", id, err)
			}
			if attempts == 1 {
				return process.Result{}, errors.New("interrupted create")
			}
		}
		return original(ctx, name, args, in)
	}
	create := func() error { return m.multiEnsureRuntime(context.Background(), j, r, true) }
	if err := create(); err == nil || attempts != 1 || len(f.containers) != 0 {
		t.Fatalf("expected interrupted create: %v", err)
	}
	different := "sha256:" + strings.Repeat("a", 64) + "\n"
	if err := os.WriteFile(path, []byte(different), 0600); err != nil {
		t.Fatal(err)
	}
	if err := create(); err == nil || attempts != 1 {
		t.Fatal("overwrote conflicting orphan receipt")
	}
	if err := os.WriteFile(path, []byte(r.Image+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := create(); err != nil || attempts != 2 {
		t.Fatalf("replay create: %v", err)
	}
}
