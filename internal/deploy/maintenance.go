package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nchapman/soar/internal/appstate"
)

func maintenanceStatus(j *journal, domain string) Status {
	s := j.Status
	if j.Maintenance != nil {
		s.Operation = j.Maintenance.Operation
	} else if j.LastMaintenanceOperation != "" {
		s.Operation = j.LastMaintenanceOperation
	}
	if isMulti(j) {
		s.URL, s.Container, s.Running = "", "", false
		s.Services = map[string]ServiceStatus{}
		for name, slot := range j.ServiceStates {
			if slot == nil {
				continue
			}
			status := ServiceStatus{Phase: slot.Phase, Unavailable: slot.Unavailable, DataMayHaveChanged: slot.DataMayHaveChanged}
			if r := slot.Current; r != nil {
				status.Kind = "private"
				status.Operation, status.Container, status.Image, status.Port = r.Operation, r.Container, r.Image, r.Port
				if r.Config != nil && r.Config.HTTP != nil {
					status.Kind = "public"
					status.URL = ServiceURL(j.App, name, domain)
				}
			}
			if slot.Candidate != nil {
				status.Candidate = slot.Candidate.Container
			}
			s.Services[name] = status
			if name == "web" {
				s.URL, s.Container = status.URL, status.Container
			}
		}
	}
	if j.Phase == "destroyed" {
		s.URL = "https://" + j.App + "." + domain
		s.Container = ""
		s.Running = false
	}
	return s
}

func (m *Manager) maintenanceFence(r Request) error {
	entries, e := os.ReadDir(m.path("/var/lib/soar/apps"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return errors.New("unexpected app entry")
		}
		j, e := m.load(entry.Name())
		if e != nil {
			return e
		}
		if j.Maintenance != nil && !(r.Action == "resume" && r.App == j.App && r.Operation == j.Maintenance.Operation) {
			return errors.New("pending maintenance requires exact resume")
		}
	}
	return nil
}

var fullResourceID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var maintenanceFile = regexp.MustCompile(`^[0-9a-f]{32}\.(run\.json|context\.tar|(?:[0-9a-f]{32}\.)?image\.iid)$`)

// Empty Service is exclusively the legacy identity. Names or prefixes alone
// never establish ownership; callers must also prove host/app/operation labels.
func maintenanceContainerIdentity(j *journal, name, service, op string) bool {
	return operation.MatchString(op) && (service == "" || appstate.ValidName(service)) &&
		(name == containerName(j.App, service, op) || (service != "" && name == "soar-run-"+op))
}

// Snapshot operation identities also retain their archives, even when a mixed
// snapshot's individual releases all originated in earlier operations.
func maintenanceRetainedOperation(j *journal, op string) bool {
	if j.Operation == op || retainedOperation(j, op) {
		return true
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil && snapshot.Operation == op {
			return true
		}
	}
	return false
}

func validateMaintenance(j *journal) error {
	bad := errors.New("invalid maintenance journal")
	if (j.LastMaintenanceOperation == "") != (j.LastMaintenanceAction == "") {
		return bad
	}
	if j.LastMaintenanceOperation != "" && (!operation.MatchString(j.LastMaintenanceOperation) || (j.LastMaintenanceAction != "cleanup" && j.LastMaintenanceAction != "destroy")) {
		return bad
	}
	w := j.Maintenance
	if w == nil {
		if j.Phase == "cleaning" || j.Phase == "destroying" {
			return bad
		}
		return nil
	}
	if !operation.MatchString(w.Operation) || maintenanceRetainedOperation(j, w.Operation) || (w.Action != "cleanup" && w.Action != "destroy") || w.Plan.Action != w.Action || w.Plan.App != j.App {
		return bad
	}
	pb, e := json.Marshal(w.Plan)
	if e != nil || len(pb) > maxMaintenancePlanBytes {
		return bad
	}
	phase := "cleaning"
	if w.Action == "destroy" {
		phase = "destroying"
	}
	if j.Phase != phase || !hasCurrent(j) || j.OldCaddy != "" || j.NewCaddy != "" {
		return bad
	}
	if w.Action == "cleanup" && (w.OldCaddy != "" || w.NewCaddy != "" || w.Network != nil) {
		return bad
	}
	if w.Action == "destroy" && (w.OldCaddy == "" || w.NewCaddy == "") {
		return bad
	}
	if len(w.Containers)+len(w.Images)+len(w.Files)+len(w.Plan.Retained) > maxMaintenancePlanItems {
		return bad
	}
	seen := map[string]bool{}
	for _, v := range w.Containers {
		if !operation.MatchString(v.Operation) || !maintenanceContainerIdentity(j, v.Name, v.Service, v.Operation) || !fullResourceID.MatchString(v.ID) || !fullResourceID.MatchString(v.NetworkID) || !imageID.MatchString(v.Image) || seen[v.ID] {
			return bad
		}
		seen[v.ID] = true
		if w.Action == "cleanup" && retainedContainer(j, v.Name) {
			return bad
		}
	}
	for _, v := range w.Images {
		if !imageID.MatchString(v) {
			return bad
		}
		if w.Action == "cleanup" {
			for _, image := range j.BuildImages {
				if image == v {
					return bad
				}
			}
			for _, r := range retainedReleases(j) {
				if r != nil && r.Image == v {
					return bad
				}
			}
		}
	}
	if len(w.Plan.Archives) != len(w.Files) {
		return bad
	}
	for i, v := range w.Files {
		if len(w.Plan.Archives) != len(w.Files) || w.Plan.Archives[i] != v.Name {
			return bad
		}
		if !maintenanceFile.MatchString(v.Name) || !fullResourceID.MatchString(v.SHA256) {
			return bad
		}
		if w.Action == "cleanup" && (v.Name == j.Archive || strings.HasSuffix(v.Name, ".image.iid") || maintenanceRetainedOperation(j, strings.Split(v.Name, ".")[0])) {
			return bad
		}
	}
	if (w.Network == nil && w.Plan.Network != "") || (w.Network != nil && w.Plan.Network != w.Network.Name) {
		return bad
	}
	if w.Network != nil && (w.Network.Name != "soar-"+j.App || !fullResourceID.MatchString(w.Network.ID)) {
		return bad
	}
	if len(w.Plan.Containers) != len(w.Containers) || len(w.Plan.Images) != len(w.Images) {
		return bad
	}
	for i, v := range w.Containers {
		if w.Plan.Containers[i] != v.Name {
			return bad
		}
	}
	for i, v := range w.Images {
		if w.Plan.Images[i] != v {
			return bad
		}
	}
	return nil
}

func (m *Manager) destroyRoutes(j *journal) (string, string, error) {
	old, e := m.routes(j, false)
	if e != nil {
		return "", "", e
	}
	clone := *j
	clone.Current = nil
	clone.Candidate = nil
	// Do not mutate the original slots: their current releases still authorize
	// resource checks after all public routes have been withdrawn.
	if isMulti(j) {
		clone.ServiceStates = nil
	}
	next, e := m.routes(&clone, true)
	return old, next, e
}

// Check both durable and live configurations. A reload acknowledgement alone is
// never authorization to stop a container.
func (m *Manager) maintenanceRoutes(c context.Context, j *journal, apply bool) error {
	w := j.Maintenance
	old, next, e := m.destroyRoutes(j)
	if e != nil {
		return e
	}
	if w.OldCaddy != old || w.NewCaddy != next {
		return errors.New("maintenance route intent differs from journals")
	}
	p := m.path("/etc/caddy/Caddyfile")
	if e = safe(p); e != nil {
		return e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if string(b) != old && (!apply || string(b) != next) {
		return errors.New("Caddyfile drift")
	}
	a, e := m.adapt(c, old)
	if e != nil {
		return e
	}
	n, e := m.adapt(c, next)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	if !sameJSON(live, a) && (!apply || !sameJSON(live, n)) {
		return errors.New("Caddy runtime drift")
	}
	if !apply {
		return nil
	}
	temp := filepath.Join(filepath.Dir(p), ".soar-next.Caddyfile")
	defer os.Remove(temp)
	if e = atomic(temp, []byte(next), 0644); e != nil {
		return e
	}
	if _, e = m.cmd(c, "caddy", "validate", "--config", temp, "--adapter", "caddyfile"); e != nil {
		return e
	}
	if string(b) != next {
		if e = atomic(p, []byte(next), 0644); e != nil {
			return e
		}
	}
	if !sameJSON(live, n) {
		_, reloadErr := m.cmd(c, "caddy", "reload", "--config", p, "--adapter", "caddyfile")
		live, e = m.live(c)
		if e != nil {
			return errors.Join(reloadErr, e)
		}
		if !sameJSON(live, n) {
			return errors.Join(reloadErr, errors.New("route removal not proven; maintenance remains pending"))
		}
	}
	return nil
}

func (m *Manager) executeMaintenance(c context.Context, r Request, j *journal) (resp Response, err error) {
	resp.Version = 1
	defer func() {
		if err != nil {
			if j != nil && j.Maintenance != nil {
				j.LastError = diagnostic(err.Error())
				err = errors.Join(err, m.save(j))
			}
			resp.Error = diagnostic(err.Error())
		}
		if j != nil {
			s := maintenanceStatus(j, r.Host.Domain)
			if j.Current != nil {
				s.Container = j.Current.Container
				v, x := m.container(c, j, j.Current)
				s.Running = x == nil && v.State.Running
				if x != nil && j.Phase != "destroying" {
					s.LastError = diagnostic(x.Error())
				}
			}
			if isMulti(j) {
				s.Running = len(j.ServiceStates) > 0
				for name, slot := range j.ServiceStates {
					if slot == nil || slot.Current == nil {
						s.Running = false
						continue
					}
					// Current slots have known immutable config. Historical
					// snapshots used for removal need only resource ownership
					// proof and never pass through this status inspection.
					v, x := m.multiInspect(c, j, slot.Current, false, true)
					status := s.Services[name]
					status.Running = x == nil && v.State.Running
					status.Available = status.Running && runtimeRunning(v) && !slot.CurrentRetired && !slot.Unavailable && j.Phase != "destroying" && v.HostConfig.RestartPolicy.Name == "unless-stopped"
					s.Services[name] = status
					s.Running = s.Running && status.Running
					if x != nil && j.Phase != "destroying" && s.LastError == "" {
						s.LastError = "maintenance current container inspection failed"
					}
				}
			}
			resp.Status = &s
		}
	}()
	if j == nil {
		return resp, errors.New("app is unmanaged")
	}
	if j.Phase == "destroyed" {
		if r.Action == "resume" && r.Operation != j.Operation {
			return resp, errors.New("resume operation does not match")
		}
		action := r.Action
		if action == "resume" {
			action = "destroy"
		}
		resp.Plan = &Plan{Action: action, App: j.App}
		return resp, m.maintenanceVolumes(c, j, resp.Plan)
	}
	if r.Action == "resume" && j.Maintenance == nil {
		if r.Operation != j.LastMaintenanceOperation {
			return resp, errors.New("resume operation does not match")
		}
		resp.Plan = &Plan{Action: j.LastMaintenanceAction, App: j.App}
		return resp, nil
	}
	if r.Action != "resume" {
		if j.Phase != "ready" || j.Maintenance != nil {
			return resp, errors.New("maintenance requires ready app")
		}
		if r.Operation == j.LastMaintenanceOperation || maintenanceRetainedOperation(j, r.Operation) {
			return resp, errors.New("operation ID already used")
		}
		w, e := m.planMaintenance(c, j, r.Action, r.Operation)
		if e != nil {
			return resp, e
		}
		resp.Plan = &w.Plan
		if w.Action == "destroy" {
			if _, _, e = m.destroyRoutes(j); e != nil {
				return resp, e
			}
		}
		if !r.Apply {
			return resp, nil
		}
		if w.Action == "destroy" {
			w.OldCaddy, w.NewCaddy, e = m.destroyRoutes(j)
			if e != nil {
				return resp, e
			}
		}
		clone := *j
		clone.Maintenance = w
		clone.Phase = "cleaning"
		if w.Action == "destroy" {
			clone.Phase = "destroying"
		}
		if e = validateJournal(&clone, j.App); e != nil {
			return resp, e
		}
		if w.Action == "destroy" {
			if e = m.maintenanceRoutes(c, &clone, false); e != nil {
				return resp, e
			}
		}
		if e = m.save(&clone); e != nil {
			return resp, e
		}
		*j = clone
	}
	w := j.Maintenance
	if w == nil || w.Operation != r.Operation {
		return resp, errors.New("resume operation does not match")
	}
	resp.Plan = &w.Plan
	if w.Action == "destroy" {
		if err = m.maintenanceRoutes(c, j, true); err != nil {
			return
		}
	}
	if err = m.removeMaintenanceResources(c, j, w); err != nil {
		return
	}
	if w.Action == "cleanup" {
		clone := *j
		clone.Phase = "ready"
		clone.Maintenance = nil
		clone.LastMaintenanceOperation = w.Operation
		clone.LastMaintenanceAction = w.Action
		clone.LastError = ""
		if err = m.save(&clone); err == nil {
			*j = clone
		}
		return
	}
	entries, e := os.ReadDir(m.dir(j.App))
	if e != nil {
		return resp, e
	}
	for _, entry := range entries {
		if entry.Name() != "state.json" {
			return resp, errors.New("unexpected file prevents destruction completion: " + entry.Name())
		}
	}
	t := appstate.Tombstone{Version: 1, App: j.App, Operation: w.Operation, Phase: "destroyed", HostID: j.HostID}
	b, e := json.Marshal(t)
	if e != nil {
		return resp, e
	}
	if err = atomic(filepath.Join(m.dir(j.App), "state.json"), b, 0600); err != nil {
		return
	}
	*j = journal{Version: 1, Status: Status{App: t.App, Operation: t.Operation, Phase: t.Phase}, HostID: t.HostID}
	return
}
