package deploy

import (
	"context"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/process"
	"strings"
	"testing"
)

func TestRemovalIntegratedAbandonAndSettledRollback(t *testing.T) {
	for _, mode := range []string{"build", "health", "rollback", "old-snapshot", "shared-key"} {
		t.Run(mode, func(t *testing.T) {
			f := newMultiFlow(t)
			ctx := context.Background()
			deploy := func(a *config.App, op, service string) error {
				q := f.req
				q.Partial = true
				q.Config = a
				q.Operation = strings.Repeat(op, 32)
				q.Service = service
				if op != "1" {
					q.HostIdentity = f.load().HostID
					q.Bindings = map[string]string{}
					for n, s := range f.load().ServiceStates {
						if service == "" || service == n {
							q.Bindings[n] = s.Incarnation
						}
					}
				}
				_, err := f.m.Execute(ctx, q, f.bundle(a))
				return err
			}
			a := rollbackApp("1")
			if mode != "rollback" && mode != "shared-key" {
				delete(a.Services, "backend")
			}
			if mode == "shared-key" {
				cfg := a.Services["backend"]
				cfg.Build = a.Services["web"].Build
				a.Services["backend"] = cfg
			}
			if err := deploy(a, "1", ""); err != nil {
				t.Fatal(err)
			}
			target := "backend"
			switch mode {
			case "build", "health":
				a = rollbackApp("2")
				delete(a.Services, "web")
				if mode == "build" {
					f.failBuild = f.builds + 1
				} else {
					cfg := a.Services["backend"]
					cfg.HTTP = &config.HTTP{Port: 8080, Health: "/"}
					a.Services["backend"] = cfg
					f.failHealth["backend"] = true
				}
				if err := deploy(a, "2", "backend"); err == nil {
					t.Fatal("expected failed creation")
				} else {
					t.Log(err)
				}
			case "old-snapshot":
				a = rollbackApp("2")
				delete(a.Services, "web")
				if err := deploy(a, "2", "backend"); err != nil {
					t.Fatal(err)
				}
				target = "web"
			case "shared-key":
				a = rollbackApp("2")
				delete(a.Services, "backend")
				cfg := a.Services["web"]
				cfg.Build = f.load().Config.Services["backend"].Build
				a.Services["web"] = cfg
				if err := deploy(a, "2", "web"); err != nil {
					t.Fatal(err)
				}
				target = "web"
			case "rollback":
				a = rollbackApp("2")
				delete(a.Services, "backend")
				if err := deploy(a, "2", "web"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.m.Execute(ctx, rollbackRequest(f, "3", "web", true), nil); err != nil {
					t.Fatal(err)
				}
				target = "web"
			}
			j := f.load()
			id := j.ServiceStates[target].Incarnation
			inventory := newMultiMaintenanceInventory(t)
			inventory.j = j
			inventory.net.Name = network(j)
			inventory.net.Labels = map[string]string{"soar.host": j.HostID, "soar.app": j.App}
			for _, slot := range j.ServiceStates {
				for _, r := range []*release{slot.Current, slot.Candidate, slot.Retiring} {
					if r != nil {
						inventory.add(r).State.Running = true
					}
				}
			}
			originalRun := f.m.Run
			f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
				if n == "docker" {
					return inventory.m.Run(ctx, n, a, b)
				}
				return originalRun(ctx, n, a, b)
			}
			q := f.req
			q.Action = "service-remove"
			q.Service = target
			q.Operation = strings.Repeat("4", 32)
			q.Apply = true
			q.Config = nil

			if _, err := f.m.executeServiceRemoval(ctx, q, j); err != nil {
				t.Fatal(err)
			}
			j = f.load()
			if j.Removed[target] != id || j.ServiceStates[target] != nil || j.Phase != "ready" {
				t.Fatal("removal did not commit")
			}
			if err := validateJournal(j, j.App); err != nil {
				t.Fatal(err)
			}

			// Cleanup must accept the settled receipt, including an empty rollback
			// target capture, without reconstructing a removed snapshot member.
			cleanup := q
			cleanup.Action = "cleanup"
			cleanup.Service = ""
			cleanup.Operation = strings.Repeat("6", 32)
			if _, err := f.m.executeMaintenance(ctx, cleanup, j); err != nil {
				t.Fatal(err)
			}
			j = f.load()
			if j.Removed[target] != id || j.ServiceStates[target] != nil {
				t.Fatal("cleanup resurrected removed membership")
			}
			f.m.Run = originalRun
			for n, c := range f.docker.containers {
				if c.Config.Labels["soar.service"] == target {
					delete(f.docker.containers, n)
				}
			}
			f.failBuild = 0
			f.failHealth = map[string]bool{}
			survivor := "web"
			if target == "web" {
				survivor = "backend"
			}
			a = rollbackApp("5")
			delete(a.Services, target)
			if err := deploy(a, "5", survivor); err != nil {
				t.Fatal(err)
			}
			j = f.load()
			if j.Removed[target] != id || j.ServiceStates[target] != nil {
				t.Fatal("removed service resurrected")
			}
			stale := f.req
			stale.Partial = true
			stale.Service = target
			stale.Operation = strings.Repeat("7", 32)
			stale.HostIdentity = j.HostID
			stale.Bindings = map[string]string{target: id}
			stale.Config = rollbackApp("7")
			delete(stale.Config.Services, survivor)
			if _, err := f.m.Execute(ctx, stale, f.bundle(stale.Config)); err == nil {
				t.Fatal("stale removed binding recreated service")
			}
			if f.load().ServiceStates[target] != nil {
				t.Fatal("stale request resurrected service")
			}

		})
	}
}
