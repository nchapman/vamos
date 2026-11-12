package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/process"
)

// This inventory deliberately does not reconstruct runtime env/config: old
// completed legacy snapshots may not have that information. Removal still
// requires exact immutable Docker and network identities and ownership labels.
type multiMaintenanceInventory struct {
	m          *Manager
	j          *journal
	containers map[string]*resourceInspection
	images     map[string]*resourceInspection
	net        *resourceInspection
	mutations  []string
}

func newMultiMaintenanceInventory(t *testing.T) *multiMaintenanceInventory {
	t.Helper()
	m := NewManager()
	var err error
	m.Root, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	j := &journal{Version: 2, HostID: strings.Repeat("a", 32), Status: Status{App: "notes", Operation: strings.Repeat("b", 32), Phase: "ready", URL: "https://notes.example.com"}, ServiceStates: map[string]*serviceState{}}
	if err := os.MkdirAll(m.dir(j.App), 0700); err != nil {
		t.Fatal(err)
	}
	f := &multiMaintenanceInventory{m: m, j: j, containers: map[string]*resourceInspection{}, images: map[string]*resourceInspection{}}
	f.net = &resourceInspection{Name: network(j)}
	f.net.ID = strings.Repeat("f", 64)
	f.net.Driver = "bridge"
	f.net.Labels = map[string]string{"soar.host": j.HostID, "soar.app": j.App}
	f.net.Containers = map[string]json.RawMessage{}
	m.Run = func(_ context.Context, name string, args []string, _ []byte) (process.Result, error) {
		if name != "docker" || len(args) < 2 {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		kind, action := args[0], args[1]
		if action == "ls" {
			var lines []string
			switch kind {
			case "container":
				for id, v := range f.containers {
					b, _ := json.Marshal(resourceRow{ID: id, Names: strings.TrimPrefix(v.Name, "/")})
					lines = append(lines, string(b))
				}
			case "network":
				if f.net != nil {
					b, _ := json.Marshal(resourceRow{ID: f.net.ID, Name: f.net.Name})
					lines = append(lines, string(b))
				}
			case "image":
				for id := range f.images {
					lines = append(lines, id)
				}
			default:
				t.Fatalf("unexpected inventory %v", args)
			}
			return process.Result{Stdout: []byte(strings.Join(lines, "\n"))}, nil
		}
		id := args[len(args)-1]
		if action == "inspect" {
			var v *resourceInspection
			switch kind {
			case "container":
				v = f.containers[id]
				if v == nil {
					for _, c := range f.containers {
						if c.Name == "/"+id {
							v = c
						}
					}
				}
			case "network":
				v = f.net
			case "image":
				v = f.images[id]
			}
			if v == nil {
				return process.Result{}, fmt.Errorf("missing %s", kind)
			}
			b, err := json.Marshal([]*resourceInspection{v})
			return process.Result{Stdout: b}, err
		}
		f.mutations = append(f.mutations, strings.Join(args, " "))
		switch kind + " " + action {
		case "container update":
			f.containers[id].HostConfig.RestartPolicy.Name = "no"
		case "container stop":
			f.containers[id].State.Running = false
		case "container rm":
			if f.containers[id].State.Running {
				t.Fatal("removed running container")
			}
			delete(f.containers, id)
			delete(f.net.Containers, id)
		case "network rm":
			if len(f.net.Containers) != 0 {
				t.Fatal("removed occupied network")
			}
			f.net = nil
		case "image rm":
			delete(f.images, id)
		default:
			t.Fatalf("unexpected mutation %v", args)
		}
		return process.Result{}, nil
	}
	return f
}

func multiMaintenanceRelease(service, op, image string, public bool) *release {
	cfg := &config.Service{Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}, Update: config.UpdateAuto}
	r := &release{Service: service, Operation: op, Container: containerName("notes", service, op), Image: image, Config: cfg}
	if public {
		cfg.HTTP = &config.HTTP{Port: 8080, Health: "/"}
		r.Port = "14001"
		r.PublishPort = r.Port
	}
	return r
}

func (f *multiMaintenanceInventory) add(r *release) *resourceInspection {
	v := &resourceInspection{Name: "/" + r.Container, Image: r.Image}
	v.ID = fmt.Sprintf("%064x", len(f.containers)+1)
	v.Config.Image = r.Image
	v.Config.Labels = map[string]string{"soar.host": f.j.HostID, "soar.app": f.j.App, "soar.operation": r.Operation}
	if r.Service != "" {
		v.Config.Labels["soar.service"] = r.Service
	}
	v.HostConfig.NetworkMode = network(f.j)
	v.NetworkSettings.Networks = map[string]json.RawMessage{network(f.j): json.RawMessage(`{"NetworkID":"` + f.net.ID + `"}`)}
	f.containers[v.ID] = v
	f.net.Containers[v.ID] = json.RawMessage(`{}`)
	image := &resourceInspection{}
	image.ID = r.Image
	f.images[r.Image] = image
	return v
}

func (f *multiMaintenanceInventory) file(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.m.dir(f.j.App), name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMultiMaintenanceSameOperationRetainsContainersByName(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	image := "sha256:" + strings.Repeat("c", 64)
	web := multiMaintenanceRelease("web", f.j.Operation, image, true)
	worker := multiMaintenanceRelease("worker", f.j.Operation, image, false)
	f.j.ServiceStates["web"] = &serviceState{Phase: "ready", Current: web}
	f.j.ServiceStates["worker"] = &serviceState{Phase: "ready", Current: worker}
	f.add(web)
	f.add(worker)
	// Same operation is not sufficient retention for an unreferenced container.
	orphan := multiMaintenanceRelease("old", f.j.Operation, image, false)
	f.add(orphan)
	f.file(t, f.j.Operation+".context.tar", "bundle")
	f.file(t, f.j.Operation+"."+strings.Repeat("d", 32)+".image.iid", image)
	w, err := f.m.planMaintenance(context.Background(), f.j, "cleanup", strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Containers) != 1 || w.Containers[0].Name != orphan.Container || w.Containers[0].Service != "old" {
		t.Fatalf("wrong containers: %+v", w.Containers)
	}
	if len(w.Images) != 0 || len(w.Files) != 0 {
		t.Fatalf("removed shared image or operation files: %+v", w)
	}
	f.j.Maintenance, f.j.Phase = w, "cleaning"
	if err := validateMaintenance(f.j); err != nil {
		t.Fatal(err)
	}
	if err := f.m.removeMaintenanceResources(context.Background(), f.j, w); err != nil {
		t.Fatal(err)
	}
	if len(f.containers) != 2 || len(f.images) != 1 {
		t.Fatalf("lost retained resources: %+v", f.mutations)
	}
}

func TestMultiMaintenanceMixedEffectiveAndCompletedRetention(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	var releases []*release
	for i, service := range []string{"web", "worker", "worker", "web", "web", ""} {
		op := fmt.Sprintf("%032x", i+1)
		r := multiMaintenanceRelease(service, op, "sha256:"+fmt.Sprintf("%064x", i+1), service != "worker")
		releases = append(releases, r)
		f.add(r)
		f.file(t, op+".context.tar", "context")
		f.file(t, op+".image.iid", r.Image)
	}
	// Last requested config cannot describe all the effective/retained versions.
	f.j.Config = &config.App{Name: f.j.App, Services: map[string]config.Service{"web": *releases[0].Config}}
	f.j.ServiceStates["web"] = &serviceState{Current: releases[0], Candidate: releases[3], Retiring: releases[4]}
	f.j.ServiceStates["worker"] = &serviceState{Current: releases[1]}
	f.j.Completed = &completedRelease{Operation: strings.Repeat("5", 32), Services: map[string]*release{"worker": releases[2]}}
	f.file(t, f.j.Completed.Operation+".context.tar", "snapshot operation")
	// A migrated historical legacy snapshot has no known immutable config.
	releases[5].Config = nil
	f.j.PreviousCompleted = &completedRelease{Operation: releases[5].Operation, Services: map[string]*release{"web": releases[5]}}
	build := "sha256:" + strings.Repeat("9", 64)
	f.j.BuildImages = map[string]string{strings.Repeat("8", 32): build}
	v := &resourceInspection{}
	v.ID = build
	f.images[build] = v
	f.file(t, strings.Repeat("7", 32)+"."+strings.Repeat("8", 32)+".image.iid", build)
	staleOp := strings.Repeat("6", 32)
	f.file(t, staleOp+".context.tar", "unretained")
	w, err := f.m.planMaintenance(context.Background(), f.j, "cleanup", strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Containers) != 0 || len(w.Images) != 0 || len(w.Files) != 1 || w.Files[0].Name != staleOp+".context.tar" {
		t.Fatalf("incomplete reference walk: %+v", w)
	}
	f.j.Maintenance, f.j.Phase = w, "cleaning"
	if err := validateMaintenance(f.j); err != nil {
		t.Fatal(err)
	}
	// Neither build-only images nor snapshot-only images may be forged into a cleanup plan.
	for _, image := range []string{build, releases[2].Image, releases[5].Image} {
		w.Images, w.Plan.Images = []string{image}, []string{image}
		if validateMaintenance(f.j) == nil {
			t.Fatal("accepted retained image", image)
		}
	}
	w.Images, w.Plan.Images = nil, nil
	for _, r := range releases {
		old := w.Operation
		w.Operation = r.Operation
		if validateMaintenance(f.j) == nil {
			t.Fatal("reused retained operation")
		}
		w.Operation = old
	}
}

func TestMultiMaintenanceContainerIdentityAndReplay(t *testing.T) {
	for _, mode := range []string{"new", "legacy", "wrong-service", "missing-service", "empty-service", "legacy-service-label", "foreign-host", "foreign-app", "prefix", "planned-service"} {
		t.Run(mode, func(t *testing.T) {
			f := newMultiMaintenanceInventory(t)
			service := "worker"
			if mode == "legacy" || mode == "legacy-service-label" || mode == "empty-service" {
				service = ""
			}
			r := multiMaintenanceRelease(service, f.j.Operation, "sha256:"+strings.Repeat("c", 64), false)
			v := f.add(r)
			p := plannedContainer{Name: r.Container, Service: service, Operation: r.Operation, ID: v.ID, Image: r.Image, NetworkID: f.net.ID}
			switch mode {
			case "wrong-service":
				v.Config.Labels["soar.service"] = "other"
			case "missing-service":
				delete(v.Config.Labels, "soar.service")
			case "empty-service":
				v.Config.Labels["soar.service"] = ""
			case "legacy-service-label":
				v.Config.Labels["soar.service"] = "worker"
			case "foreign-host":
				v.Config.Labels["soar.host"] = strings.Repeat("d", 32)
			case "foreign-app":
				v.Config.Labels["soar.app"] = "notes-worker"
			case "prefix":
				p.Name += "-extra"
				v.Name = "/" + p.Name
			case "planned-service":
				p.Service = "other"
			}
			_, err := f.m.plannedResourceContainer(context.Background(), f.j, p)
			if (err == nil) != (mode == "new" || mode == "legacy") {
				t.Fatalf("identity boundary: %v", err)
			}
			if len(f.mutations) != 0 {
				t.Fatal(f.mutations)
			}
		})
	}
}

func TestMultiMaintenanceDestroyAllServicesAndNewIID(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	image := "sha256:" + strings.Repeat("c", 64)
	f.j.Config = &config.App{Name: f.j.App, Services: map[string]config.Service{}}
	f.j.Completed = &completedRelease{Operation: f.j.Operation, Services: map[string]*release{}}
	for i, service := range []string{"api", "web", "worker"} {
		r := multiMaintenanceRelease(service, f.j.Operation, image, service != "worker")
		if r.Config.HTTP != nil {
			r.Port = fmt.Sprint(14001 + i)
			r.PublishPort = r.Port
		}
		f.j.ServiceStates[service] = &serviceState{Phase: "ready", Current: r}
		f.j.Config.Services[service] = *r.Config
		f.j.Completed.Services[service] = r
		f.add(r).State.Running = true
	}
	f.j.ServiceOrder = []string{"api", "web", "worker"}
	key := buildcontext.BuildKey(*f.j.Config.Services["web"].Build)
	f.j.BuildImages = map[string]string{key: image}
	f.j.BundleSHA = strings.Repeat("d", 64)
	f.j.Archive = f.j.Operation + ".context.tar"
	f.file(t, f.j.Archive, "bundle")
	f.file(t, f.j.Operation+"."+key+".image.iid", image)
	if err := validateJournal(f.j, f.j.App); err != nil {
		t.Fatal(err)
	}
	if err := f.m.save(f.j); err != nil {
		t.Fatal(err)
	}

	caddy, request := flowSetup(t)
	f.m.HTTP = caddy.m.HTTP
	caddy.m = f.m
	runDocker := f.m.Run
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "caddy" {
			return caddy.run(ctx, name, args, input)
		}
		if len(args) > 1 && args[1] != "inspect" && args[1] != "ls" && caddy.live != baseCaddy {
			t.Fatal("resource mutation before route withdrawal", args)
		}
		return runDocker(ctx, name, args, input)
	}
	old, next, err := f.m.destroyRoutes(f.j)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old, "127.0.0.1:14001") || !strings.Contains(old, "127.0.0.1:14002") || next != baseCaddy {
		t.Fatal("incomplete public route set", old, next)
	}
	caddy.live = old
	if err := atomic(f.m.path("/etc/caddy/Caddyfile"), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	request.Action, request.Operation, request.Config, request.Apply = "destroy", strings.Repeat("e", 32), nil, true
	caddy.reloadCrash = "before"
	if _, err := f.m.executeMaintenance(context.Background(), request, f.j); err == nil {
		t.Fatal("unproven route withdrawal accepted")
	}
	if len(f.mutations) != 0 {
		t.Fatal("resources removed before route proof", f.mutations)
	}
	loaded, err := f.m.load(f.j.App)
	if err != nil {
		t.Fatal("invalid persisted maintenance intent", err)
	}
	f.j = loaded
	w := f.j.Maintenance
	if w == nil || len(w.Containers) != 3 || len(w.Images) != 1 || len(w.Files) != 2 || w.Network == nil {
		t.Fatalf("incomplete destroy: %+v", w)
	}
	caddy.reloadCrash, caddy.adminFail = "", false
	request.Action = "resume"
	response, err := f.m.executeMaintenance(context.Background(), request, f.j)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status.Phase != "destroyed" || f.j.Version != 1 || len(f.j.ServiceStates) != 0 {
		t.Fatal("not a minimal v1 receipt", response.Status)
	}
	if len(f.containers) != 0 || len(f.images) != 0 || f.net != nil {
		t.Fatal("resources remain", f.mutations)
	}
	entries, err := os.ReadDir(f.m.dir(f.j.App))
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("IID/archive remain: %v %v", entries, err)
	}
	if _, err := f.m.load(f.j.App); err != nil {
		t.Fatal("invalid destruction receipt", err)
	}
	if _, err := f.m.executeMaintenance(context.Background(), request, f.j); err != nil {
		t.Fatal("non-idempotent receipt replay", err)
	}
}

func TestMultiMaintenancePrivateOnlyReady(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	r := multiMaintenanceRelease("worker", f.j.Operation, "sha256:"+strings.Repeat("c", 64), false)
	f.j.ServiceStates["worker"] = &serviceState{Phase: "ready", Current: r}
	f.j.Maintenance = &maintenance{Operation: strings.Repeat("e", 32), Action: "cleanup", Plan: Plan{Action: "cleanup", App: f.j.App}}
	f.j.Phase = "cleaning"
	if err := validateMaintenance(f.j); err != nil {
		t.Fatal("private-only current rejected", err)
	}
	s := maintenanceStatus(f.j, "example.com")
	if s.URL != "" || s.Container != "" || s.Services["worker"].Container != r.Container {
		t.Fatalf("bad private-only status: %+v", s)
	}
}

func TestMultiMaintenanceDestroyRemovesEveryPublicRoute(t *testing.T) {
	f, request := flowSetup(t)
	j := f.execute(request, "archive", false)
	web := multiMaintenanceRelease("web", j.Operation, j.Current.Image, true)
	api := multiMaintenanceRelease("api", j.Operation, j.Current.Image, true)
	api.Port, api.PublishPort = "14002", "14002"
	worker := multiMaintenanceRelease("worker", j.Operation, j.Current.Image, false)
	j.Version, j.Current, j.Previous, j.Candidate = 2, nil, nil, nil
	j.ServiceStates = map[string]*serviceState{"web": {Current: web}, "api": {Current: api}, "worker": {Current: worker}}
	j.Config = &config.App{Name: j.App, Services: map[string]config.Service{"web": *web.Config, "api": *api.Config, "worker": *worker.Config}}
	old, next, err := f.m.destroyRoutes(j)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old, "127.0.0.1:14001") || !strings.Contains(old, "127.0.0.1:14002") || next != baseCaddy {
		t.Fatalf("routes not removed: %q -> %q", old, next)
	}
	if j.ServiceStates["web"].Current != web || j.ServiceStates["api"].Current != api {
		t.Fatal("route preview mutated retained releases")
	}
	f.live = old
	if err := atomic(f.m.path("/etc/caddy/Caddyfile"), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	j.Maintenance = &maintenance{OldCaddy: old, NewCaddy: next}
	if err := f.m.maintenanceRoutes(context.Background(), j, true); err != nil {
		t.Fatal(err)
	}
	if f.live != next {
		t.Fatal("route withdrawal was not proven")
	}
	// Private-only destruction is a legitimate no-route transition.
	j.ServiceStates = map[string]*serviceState{"worker": {Current: worker}}
	j.Config.Services = map[string]config.Service{"worker": *worker.Config}
	old, next, err = f.m.destroyRoutes(j)
	if err != nil || old != baseCaddy || next != baseCaddy {
		t.Fatalf("private-only route removal: %q %q %v", old, next, err)
	}
}

func TestMultiMaintenanceProtectsOtherAppReferences(t *testing.T) {
	for _, field := range []string{"current", "candidate", "retiring", "completed", "previous-completed", "build"} {
		t.Run(field, func(t *testing.T) {
			f := newMultiMaintenanceInventory(t)
			target := "sha256:" + strings.Repeat("c", 64)
			otherImage := "sha256:" + strings.Repeat("d", 64)
			current := multiMaintenanceRelease("web", strings.Repeat("1", 32), otherImage, true)
			current.Container = containerName("other", current.Service, current.Operation)
			r := multiMaintenanceRelease("web", strings.Repeat("2", 32), target, true)
			r.Container = containerName("other", r.Service, r.Operation)
			other := &journal{Version: 2, HostID: f.j.HostID, Status: Status{App: "other", Operation: strings.Repeat("3", 32), Phase: "updating", URL: "https://other.example.com"}, BundleSHA: strings.Repeat("4", 64)}
			other.Archive = other.Operation + ".context.tar"
			other.Config = &config.App{Name: other.App, Services: map[string]config.Service{"web": *current.Config}}
			key := buildcontext.BuildKey(*current.Config.Build)
			other.BuildImages = map[string]string{key: otherImage}
			slot := &serviceState{Phase: "ready", Current: current}
			other.ServiceStates = map[string]*serviceState{"web": slot}
			other.ServiceOrder = []string{"web"}
			other.Completed = &completedRelease{Operation: current.Operation, Services: map[string]*release{"web": current}}
			switch field {
			case "current":
				slot.Current = r
			case "candidate":
				r.Operation = other.Operation
				r.Container = containerName(other.App, r.Service, r.Operation)
				slot.Phase, slot.Candidate = "prepared", r
				other.BuildImages[key] = target
			case "retiring":
				prior := *current
				other.Completed.Services["web"] = &prior
				current.Operation = other.Operation
				current.Container = containerName(other.App, current.Service, current.Operation)
				slot.Phase, slot.Retiring = "draining", r
			case "completed":
				other.Completed = &completedRelease{Operation: r.Operation, Services: map[string]*release{"web": r}}
			case "previous-completed":
				// Historical migrated v1 data need not carry runtime config.
				r.Service, r.Config = "", nil
				r.Container = containerName(other.App, "", r.Operation)
				other.PreviousCompleted = &completedRelease{Operation: r.Operation, Services: map[string]*release{"web": r}}
			case "build":
				other.Phase = "building"
				other.Completed = nil
				slot.Phase, slot.Current, slot.New = "pending", nil, true
				other.BuildImages[key] = target
			}
			if err := validateJournal(other, other.App); err != nil {
				t.Fatal("invalid cross-app fixture", err)
			}
			if err := f.m.save(other); err != nil {
				t.Fatal(err)
			}
			v := &resourceInspection{}
			v.ID = target
			f.images[target] = v
			f.file(t, f.j.Operation+".image.iid", target)
			w, err := f.m.planMaintenance(context.Background(), f.j, "destroy", strings.Repeat("e", 32))
			if err != nil {
				t.Fatal(err)
			}
			if len(w.Images) != 0 || len(w.Plan.Retained) != 1 {
				t.Fatalf("cross-app image not protected: %+v", w)
			}
			// A previously computed plan must also honor newly appearing references.
			if err := f.m.removeMaintenanceResources(context.Background(), f.j, &maintenance{Action: "destroy", Images: []string{target}}); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 0 {
				t.Fatal("removed newly shared image", f.mutations)
			}
		})
	}
}

func TestMultiMaintenanceIIDProvenanceBoundaries(t *testing.T) {
	f := newMultiMaintenanceInventory(t)
	f.j.ServiceStates["worker"] = &serviceState{Current: multiMaintenanceRelease("worker", f.j.Operation, "sha256:"+strings.Repeat("c", 64), false)}
	op, build := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for _, name := range []string{op + ".image.iid", op + "." + build + ".image.iid"} {
		if !maintenanceFile.MatchString(name) || resourceFile.FindStringSubmatch(name) == nil {
			t.Fatal("supported provenance name rejected", name)
		}
		w := &maintenance{Operation: strings.Repeat("e", 32), Action: "destroy", OldCaddy: "old", NewCaddy: "new", Plan: Plan{Action: "destroy", App: f.j.App, Archives: []string{name}}, Files: []plannedFile{{Name: name, SHA256: strings.Repeat("f", 64)}}}
		f.j.Maintenance, f.j.Phase = w, "destroying"
		if err := validateMaintenance(f.j); err != nil {
			t.Fatal(err)
		}
		w.Action, w.Plan.Action, w.OldCaddy, w.NewCaddy = "cleanup", "cleanup", "", ""
		f.j.Phase = "cleaning"
		if validateMaintenance(f.j) == nil {
			t.Fatal("cleanup accepted provenance deletion", name)
		}
		if err := f.m.removeMaintenanceResources(context.Background(), f.j, w); err == nil {
			t.Fatal("cleanup removed provenance", name)
		}
	}
	for _, name := range []string{op + "." + build + ".context.tar", op + ".bad.image.iid", "../" + op + ".image.iid", op + "." + build + ".extra.image.iid"} {
		if maintenanceFile.MatchString(name) || resourceFile.FindStringSubmatch(name) != nil {
			t.Fatal("unsafe filename accepted", name)
		}
	}
}
