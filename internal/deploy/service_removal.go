package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nchapman/soar/internal/config"
	"maps"
	"reflect"
)

// validateServiceAbandonment proves a single initial creation can be discarded.
// It deliberately uses the captured effective config, never a replacement request.
// Validators may also use this proof for the durable removing phase, without
// pretending the original failed slot was ready or had a healthy Current.
func validateServiceAbandonment(j *journal, name string) error {
	refuse := errors.New("cannot safely abandon this service; resume the captured deployment operation first")
	if j == nil || j.Version != 7 || j.Config == nil || len(j.ServiceStates) < 2 || len(j.Selected) != 1 || j.Selected[0] != name || j.Rollback != nil || j.OldCaddy != "" || j.NewCaddy != "" || j.Completed == nil {
		return refuse
	}
	switch j.Phase {
	case "building", "updating":
		if j.Maintenance != nil {
			return refuse
		}
	case "removing":
		if j.Maintenance == nil || j.Maintenance.Action != "service-remove" || j.Maintenance.Service != name {
			return refuse
		}
	default:
		return refuse
	}
	selected := j.ServiceStates[name]
	if selected == nil || !selected.New || selected.Skipped || selected.Previous != nil || selected.Retiring != nil || selected.CurrentRetired || j.Completed.Services[name] != nil {
		return refuse
	}
	if selected.Current != nil && selected.Current.Operation != j.Operation {
		return refuse
	}
	if selected.Candidate != nil && selected.Candidate.Operation != j.Operation {
		return refuse
	}
	// A failed build leaves even untouched slots pending; their immutable
	// Current and completed snapshot still prove the effective runtime.
	for n, slot := range j.ServiceStates {
		if n == name {
			continue
		}
		cfg, ok := j.Config.Services[n]
		if !ok || slot == nil || !slot.Skipped || (slot.Phase != "ready" && slot.Phase != "pending") || slot.Current == nil || slot.Current.Config == nil || slot.New || slot.CurrentRetired || slot.Unavailable || slot.Candidate != nil || slot.Retiring != nil || slot.Current.Operation == j.Operation || !reflect.DeepEqual(cfg, *slot.Current.Config) || !maps.Equal(slot.SecretRefs, slot.Current.SecretRefs) || !reflect.DeepEqual(j.Completed.Services[n], slot.Current) {
			return refuse
		}
	}
	return nil
}

func validateServiceRemoval(j *journal) error {
	w := j.Maintenance
	if j.Version != 7 || w.Service == "" || j.ServiceStates[w.Service] == nil || len(j.ServiceStates) < 2 || w.Network != nil || len(w.Images) != 0 || len(w.Files) != 0 || w.OldCaddy == "" || w.NewCaddy == "" {
		return errors.New("invalid service removal intent")
	}
	for _, c := range w.Containers {
		if c.Service != w.Service {
			return errors.New("service removal crosses service boundary")
		}
	}
	return nil
}

func (m *Manager) maintenanceRouteIntent(j *journal) (string, string, error) {
	if j.Maintenance == nil || j.Maintenance.Action != "service-remove" {
		return m.destroyRoutes(j)
	}
	old, e := m.routes(j, false)
	if e != nil {
		return "", "", e
	}
	clone := *j
	clone.ServiceStates = make(map[string]*serviceState)
	for n, s := range j.ServiceStates {
		if n != j.Maintenance.Service {
			clone.ServiceStates[n] = s
		}
	}
	next, e := m.routes(&clone, true)
	return old, next, e
}

// Removal accepts settled apps or a proven isolated incomplete initial creation.
// Pending route intent is never silently abandoned.
func (m *Manager) executeServiceRemoval(ctx context.Context, r Request, j *journal) (resp Response, err error) {
	resp.Version = 1
	defer func() {
		if err != nil {
			resp.Error = diagnostic(err.Error())
			if j != nil && j.Maintenance != nil {
				j.LastError = resp.Error
				err = errors.Join(err, m.save(j))
			}
		}
		if j != nil {
			s := maintenanceStatus(j, r.Host.Domain)
			resp.Status = &s
		}
	}()
	if j == nil {
		return resp, errors.New("app is unmanaged")
	}
	if r.Action != "resume" {
		if j.Maintenance != nil || (j.Rollback != nil && j.Phase != "ready") || j.OldCaddy != "" || j.NewCaddy != "" {
			return resp, errors.New("service removal requires reconciliation; resume the captured operation first")
		}
		abandoning := j.Phase != "ready"
		if abandoning {
			if e := validateServiceAbandonment(j, r.Service); e != nil {
				return resp, e
			}
		}
		if !isMulti(j) || j.Config == nil {
			return resp, errors.New("service removal requires multi-service app; use destroy")
		}
		cfg, ok := j.Config.Services[r.Service]
		if !ok || r.Service == "" {
			return resp, errors.New("service is required and must exist")
		}
		if len(j.ServiceStates) < 2 {
			return resp, errors.New("last service cannot be removed; use destroy")
		}
		if r.Apply && cfg.Update == config.UpdateManual && !r.AllowProtected {
			return resp, errors.New("manual service removal requires allow-protected")
		}
		if r.Operation == j.LastMaintenanceOperation || maintenanceRetainedOperation(j, r.Operation) {
			return resp, errors.New("operation ID already used")
		}
		planSource := j
		if abandoning {
			// Only the planner's settled-state gate is bypassed. Actual journal and
			// removal validation still see the incomplete slot and captured releases.
			b, e := json.Marshal(j)
			if e != nil {
				return resp, e
			}
			var copy journal
			if e = json.Unmarshal(b, &copy); e != nil {
				return resp, e
			}
			copy.Phase = "ready"
			planSource = &copy
		}
		w, e := m.planMaintenance(ctx, planSource, "destroy", r.Operation)
		if e != nil {
			return resp, e
		}
		w.Action = "service-remove"
		w.Service = r.Service
		w.Plan.Action = w.Action
		w.Network = nil
		w.Plan.Network = ""
		w.Images = nil
		w.Plan.Images = nil
		w.Files = nil
		w.Plan.Archives = nil
		cs := w.Containers
		w.Containers = nil
		w.Plan.Containers = nil
		for _, c := range cs {
			if c.Service == r.Service {
				w.Containers = append(w.Containers, c)
				w.Plan.Containers = append(w.Plan.Containers, c.Name)
			}
		}
		w.Plan.Retained = append(w.Plan.Retained, "service data, secrets, shared network, images and archives retained")
		resp.Plan = &w.Plan
		if !r.Apply {
			return resp, nil
		}
		b, _ := json.Marshal(j)
		var clone journal
		if e = json.Unmarshal(b, &clone); e != nil {
			return resp, e
		}
		upgradeServiceJournal(&clone)
		clone.Maintenance = w
		clone.Phase = "removing"
		w.OldCaddy, w.NewCaddy, e = m.maintenanceRouteIntent(&clone)
		if e != nil {
			return resp, e
		}
		if e = validateJournal(&clone, j.App); e != nil {
			if abandoning {
				return resp, fmt.Errorf("cannot persist safe service abandonment; resume the captured deployment operation first: %w", e)
			}
			return resp, e
		}
		if e = m.maintenanceRoutes(ctx, &clone, false); e != nil {
			return resp, e
		}
		if e = m.save(&clone); e != nil {
			return resp, e
		}
		*j = clone
	}
	w := j.Maintenance
	if w == nil || w.Action != "service-remove" || w.Operation != r.Operation {
		return resp, errors.New("resume operation does not match")
	}
	resp.Plan = &w.Plan
	if err = m.maintenanceRoutes(ctx, j, true); err != nil {
		return
	}
	if err = m.removeMaintenanceResources(ctx, j, w); err != nil {
		return
	}
	b, _ := json.Marshal(j)
	var clone journal
	if err = json.Unmarshal(b, &clone); err != nil {
		return
	}
	commitServiceRemoval(&clone)
	if err = m.save(&clone); err == nil {
		*j = clone
	}
	return
}

func commitServiceRemoval(j *journal) {
	w := j.Maintenance
	name := w.Service
	abandoning := validateServiceAbandonment(j, name) == nil
	if j.Removed == nil {
		j.Removed = map[string]string{}
	}
	j.Removed[name] = serviceIncarnation(j, name)
	delete(j.Config.Services, name)
	delete(j.ServiceStates, name)
	// BuildImages describes this operation's selected sources, not every
	// surviving service that happens to use the same build/image key.
	sources := activeSources(j)
	for key := range j.BuildImages {
		if _, active := sources[key]; !active {
			delete(j.BuildImages, key)
		}
	}
	for _, s := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if s != nil {
			delete(s.Services, name)
		}
	}
	filter := func(in []string) []string {
		out := []string{}
		for _, n := range in {
			if n != name {
				out = append(out, n)
			}
		}
		return out
	}
	if j.PreviousCompleted != nil && len(j.PreviousCompleted.Services) == 0 {
		j.PreviousCompleted = nil
	}
	if j.Rollback != nil {
		delete(j.Rollback.Targets, name)
	}
	if abandoning {
		j.Completed = &completedRelease{Operation: j.Operation, Services: map[string]*release{}}
		for n, slot := range j.ServiceStates {
			slot.Phase = "ready"
			j.Completed.Services[n] = slot.Current
		}
	}
	j.Selected = filter(j.Selected)
	j.ServiceOrder = filter(j.ServiceOrder)
	if j.Scope == name {
		j.Scope = ""
		if j.Rollback != nil {
			j.Rollback.AllowDowntime = false
		}
	}
	if j.ActiveService == name {
		j.ActiveService = ""
	}
	j.Phase = "ready"
	j.Maintenance = nil
	j.LastMaintenanceOperation = w.Operation
	j.LastMaintenanceAction = w.Action
	j.LastError = ""
}
