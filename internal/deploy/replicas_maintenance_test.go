package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

func replicaMaintenance(t *testing.T) *multiMaintenanceInventory {
	f := newMultiMaintenanceInventory(t)
	f.j.Version = 8
	f.j.Config = &config.App{Name: f.j.App, Services: map[string]config.Service{}}
	f.j.Completed = &completedRelease{Operation: f.j.Operation, Services: map[string]*release{}}
	for serviceIndex, name := range []string{"api", "web"} {
		r := multiMaintenanceRelease(name, f.j.Operation, "sha256:"+strings.Repeat("c", 64), true)
		n := 3
		r.Config.Replicas = &n
		r.Container = ""
		r.Port = ""
		r.PublishPort = ""
		for ordinal := 1; ordinal <= 3; ordinal++ {
			port := fmt.Sprint(14000 + serviceIndex*3 + ordinal)
			member := releaseInstance{Ordinal: ordinal, Container: instanceName(f.j.App, name, r.Operation, ordinal), Port: port, PublishPort: port}
			r.Instances = append(r.Instances, member)
			// The inventory boundary stores Docker objects, not duplicated captured configs.
			v := f.add(&release{Container: member.Container, Service: name, Operation: r.Operation, Image: r.Image})
			v.Config.Labels["vamos.replica"] = fmt.Sprint(ordinal)
			v.State.Running = true
		}
		f.j.ServiceStates[name] = &serviceState{Phase: "ready", Current: r, Incarnation: strings.Repeat("a", 32)}
		f.j.Config.Services[name] = *r.Config
		f.j.Completed.Services[name] = r
	}
	f.j.Selected = []string{"api", "web"}
	f.j.ServiceOrder = []string{"api", "web"}
	key := buildcontext.BuildKey(*f.j.Config.Services["web"].Build)
	image := f.j.ServiceStates["web"].Current.Image
	f.j.BuildImages = map[string]string{key: image}
	f.j.Archive = f.j.Operation + ".context.tar"
	f.j.BundleSHA = strings.Repeat("d", 64)
	f.file(t, f.j.Archive, "bundle")
	f.file(t, f.j.Operation+"."+key+".image.iid", image)
	if err := validateJournal(f.j, f.j.App); err != nil {
		t.Fatal(err)
	}
	if err := f.m.save(f.j); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReplicaMaintenanceRetentionDestroyAndRemove(t *testing.T) {
	for _, action := range []string{"cleanup", "destroy", "service-remove"} {
		t.Run(action, func(t *testing.T) {
			f := replicaMaintenance(t)
			caddy, q := flowSetup(t)
			f.m.HTTP = caddy.m.HTTP
			caddy.m = f.m
			docker := f.m.Run
			failed := false
			f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
				if n == "caddy" {
					return caddy.run(c, n, a, b)
				}
				result, err := docker(c, n, a, b)
				if action != "cleanup" && n == "docker" && len(a) > 1 && a[0] == "container" && a[1] == "rm" && !failed && err == nil {
					failed = true
					return result, fmt.Errorf("lost member removal")
				}
				return result, err
			}
			routes, err := f.m.routes(f.j, false)
			if err != nil {
				t.Fatal(err)
			}
			caddy.live = routes
			if err := atomic(f.m.path("/etc/caddy/Caddyfile"), []byte(routes), 0644); err != nil {
				t.Fatal(err)
			}
			q.Action = action
			q.Config = nil
			q.ContextFormat = ""
			q.Operation = strings.Repeat("e", 32)
			q.Apply = true
			execute := f.m.executeMaintenance
			if action == "service-remove" {
				q.Service = "api"
				execute = f.m.executeServiceRemoval
			}
			_, err = execute(context.Background(), q, f.j)
			if action != "cleanup" {
				if err == nil || !failed {
					t.Fatal("expected interrupted member deletion", err)
				}
				f.j, err = f.m.load(f.j.App)
				if err != nil {
					t.Fatal(err)
				}
				q.Action = "resume"
				_, err = execute(context.Background(), q, f.j)
			}
			if err != nil {
				t.Fatal(err)
			}
			wanted := 6
			if action == "destroy" {
				wanted = 0
			}
			if action == "service-remove" {
				wanted = 3
			}
			if len(f.containers) != wanted {
				t.Fatalf("containers remaining %d want %d", len(f.containers), wanted)
			}
			if action != "destroy" && (f.j.Version != 8 || len(f.images) != 1) {
				t.Fatal("downgrade or shared image removed")
			}
			if action == "service-remove" && (strings.Contains(caddy.live, "14001") || !strings.Contains(caddy.live, "14006")) {
				t.Fatal("wrong remaining upstream set")
			}
		})
	}
}

func TestReplicaMaintenanceRejectsUnrecordedMember(t *testing.T) {
	f := replicaMaintenance(t)
	r := f.j.ServiceStates["web"].Current
	phantom := f.add(&release{Container: instanceName(f.j.App, "web", r.Operation, 4), Service: "web", Operation: r.Operation, Image: r.Image})
	phantom.Config.Labels["vamos.replica"] = "4"
	if _, err := f.m.planMaintenance(context.Background(), f.j, "destroy", strings.Repeat("e", 32)); err == nil {
		t.Fatal("unrecorded extra member authorized")
	}
	if len(f.mutations) != 0 {
		t.Fatal("mutated foreign member")
	}
}

func TestReplicaMaintenanceRejectsWrongOrdinal(t *testing.T) {
	for _, label := range []string{"", "0", "4", "01", "2"} {
		f := replicaMaintenance(t)
		for _, v := range f.containers {
			if strings.HasSuffix(v.Name, "-1") {
				v.Config.Labels["vamos.replica"] = label
				break
			}
		}
		if _, err := f.m.planMaintenance(context.Background(), f.j, "destroy", strings.Repeat("e", 32)); err == nil {
			t.Fatalf("accepted wrong replica label %q", label)
		}
		if len(f.mutations) != 0 {
			t.Fatal("mutated unowned member")
		}
	}
}
