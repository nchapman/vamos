package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/secrets"
)

type RollbackService struct {
	CurrentDomains []string          `json:"current_domains,omitempty"`
	TargetDomains  []string          `json:"target_domains,omitempty"`
	Name           string            `json:"name"`
	CurrentImage   string            `json:"current_image,omitempty"`
	TargetImage    string            `json:"target_image,omitempty"`
	ExcludedReason string            `json:"excluded_reason,omitempty"`
	Downtime       bool              `json:"downtime"`
	SecretVersions map[string]string `json:"secret_versions,omitempty"`
}
type RollbackPreview struct {
	TargetOperation   string            `json:"target_operation"`
	NeedsConfirmation bool              `json:"needs_confirmation"`
	Services          []RollbackService `json:"services"`
}

// Targets are per service, not source key: scoped releases may contain different
// immutable images built from the same Dockerfile at different times.
type rollbackCapture struct {
	TargetOperation string
	AllowDowntime   bool
	Targets         map[string]*release
}

func selectedImage(j *journal, name string) string {
	if j.Rollback != nil {
		if r := j.Rollback.Targets[name]; r != nil {
			return r.Image
		}
		return ""
	}
	return j.BuildImages[imageKey(j.Config.Services[name])]
}

func rollbackPlan(r Request, old *journal) (*journal, *RollbackPreview, error) {
	if !isMulti(old) || old.Phase == "destroyed" {
		return nil, nil, errors.New("rollback requires captured multi-service release history")
	}
	if old.Phase != "ready" && old.Phase != "building" && old.Phase != "releasing" && old.Phase != "updating" {
		return nil, nil, errors.New("finish maintenance before rollback")
	}
	if old.NewCaddy != "" {
		return nil, nil, errors.New("routing transition is pending; resume before rollback")
	}
	for _, s := range old.ServiceStates {
		if s.Phase == "draining" || s.Phase == "withdrawing" {
			return nil, nil, errors.New("routing retirement is pending; resume before rollback")
		}
	}
	if r.Service != "" && old.ServiceStates[r.Service] == nil {
		return nil, nil, errors.New("selected service has no recorded deployment")
	}
	target := old.PreviousCompleted
	if old.Phase != "ready" {
		target = old.Completed
	}
	// A ready v7 scoped rollback follows that service's history, not the
	// last app-wide update (which may have changed only another service).
	if old.Version >= 7 && old.Phase == "ready" && r.Service != "" {
		previous := old.ServiceStates[r.Service].Previous
		if previous == nil || reflect.DeepEqual(previous, old.ServiceStates[r.Service].Current) {
			return nil, nil, errors.New("selected service has no previous completed release available for rollback")
		}
		target = &completedRelease{Operation: previous.Operation, Services: map[string]*release{r.Service: previous}}
	}
	if target == nil {
		return nil, nil, errors.New("no previous completed release is available for rollback")
	}
	next := &journal{Version: 5, Status: Status{App: old.App, URL: old.URL, Operation: r.Operation, Phase: "building"}, HostID: old.HostID, Scope: r.Service, ReuseData: old.ReuseData,
		Config: &config.App{Name: old.App, Services: map[string]config.Service{}}, ServiceStates: map[string]*serviceState{}, Completed: old.Completed, PreviousCompleted: old.PreviousCompleted,
		Rollback: &rollbackCapture{TargetOperation: target.Operation, AllowDowntime: r.AllowDowntime, Targets: map[string]*release{}}}
	if old.Version >= 6 {
		next.Version = old.Version
	}
	if old.Version >= 7 {
		next.Removed = maps.Clone(old.Removed)
		next.HostIdentity = old.HostIdentity
	}
	preview := &RollbackPreview{TargetOperation: target.Operation, NeedsConfirmation: !r.Apply}
	names := make([]string, 0, len(old.ServiceStates))
	for name := range old.ServiceStates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prior := old.ServiceStates[name]
		wanted := target.Services[name]
		item := RollbackService{Name: name}
		if prior.Current != nil {
			item.CurrentImage = prior.Current.Image
			if prior.Current.Config.HTTP != nil {
				item.CurrentDomains = prior.Current.Config.HTTP.Domains
			}
		}
		if wanted != nil {
			item.TargetImage = wanted.Image
			if wanted.Config != nil && wanted.Config.HTTP != nil {
				item.TargetDomains = wanted.Config.HTTP.Domains
			}
		}
		currentCfg := old.Config.Services[name]
		if prior.Current != nil {
			currentCfg = *prior.Current.Config
		}
		switch {
		case r.Service != "" && r.Service != name:
			item.ExcludedReason = "outside selected scope"
		case currentCfg.Update == config.UpdateManual || (wanted != nil && wanted.Config != nil && wanted.Config.Update == config.UpdateManual):
			item.ExcludedReason = "manual service; use an explicit scoped deploy"
		case wanted == nil:
			item.ExcludedReason = "not present in target release; service removal is not implicit"
		case wanted.Config == nil:
			item.ExcludedReason = "historical configuration was not captured"
		case currentCfg.Volume != nil || wanted.Config.Volume != nil:
			if r.Service != name || !r.AllowDowntime {
				item.ExcludedReason = "volume-backed service; select it with --allow-downtime and confirm data compatibility"
			}
		}
		if item.ExcludedReason != "" {
			preview.Services = append(preview.Services, item)
			if r.Service == name {
				return nil, preview, fmt.Errorf("service %s: %s", name, item.ExcludedReason)
			}
			if prior.Current == nil || prior.CurrentRetired || prior.Unavailable {
				return nil, preview, fmt.Errorf("excluded service %s has incomplete recovery; resume or supersede first", name)
			}
			next.Config.Services[name] = currentCfg
			next.ServiceStates[name] = &serviceState{Phase: "pending", Current: prior.Current, Skipped: true, SecretRefs: maps.Clone(prior.Current.SecretRefs), VolumeID: prior.VolumeID, DataMayHaveChanged: prior.DataMayHaveChanged}
			continue
		}
		if !reflect.DeepEqual(currentCfg.Volume, wanted.Config.Volume) || prior.VolumeID != wanted.VolumeID || (currentCfg.HTTP == nil) != (wanted.Config.HTTP == nil) {
			return nil, preview, fmt.Errorf("service %s: rollback cannot change storage identity or public/private kind", name)
		}
		next.Config.Services[name] = *wanted.Config
		next.Rollback.Targets[name] = wanted
		next.ServiceStates[name] = &serviceState{Phase: "pending", Current: prior.Current, CurrentRetired: prior.CurrentRetired, Unavailable: prior.Unavailable, New: prior.Current == nil, SecretRefs: maps.Clone(wanted.SecretRefs), VolumeID: prior.VolumeID, DataMayHaveChanged: prior.DataMayHaveChanged}
		item.Downtime = wanted.Config.HTTP == nil || wanted.Config.Volume != nil
		item.SecretVersions = map[string]string{}
		for env, ref := range wanted.SecretRefs {
			item.SecretVersions[env] = ref.Version
		}
		preview.Services = append(preview.Services, item)
	}
	if next.Version >= 7 {
		for _, name := range names {
			next.ServiceStates[name].Incarnation = old.ServiceStates[name].Incarnation
			next.ServiceStates[name].Previous = old.ServiceStates[name].Previous
			if next.Rollback.Targets[name] != nil {
				next.Selected = append(next.Selected, name)
			}
		}
	}
	next.ServiceOrder = serviceOrder(next)
	if len(next.Rollback.Targets) == 0 {
		preview.NeedsConfirmation = false
		return nil, preview, nil
	}
	if err := validateMultiJournal(next, next.App); err != nil {
		return nil, preview, err
	}
	return next, preview, nil
}

// Preflight every selected image and secret before retiring any old runtime.
// Missing immutable inputs fail closed; never rebuild, pull, or use staged values.
func (m *Manager) rollbackInputs(ctx context.Context, j *journal) error {
	for _, name := range j.ServiceOrder {
		r := j.Rollback.Targets[name]
		if r == nil {
			continue
		}
		if _, err := m.secretValues(j, r.SecretRefs); err != nil {
			return fmt.Errorf("service %s: captured secret version unavailable", name)
		}
		im, err := m.inspect(ctx, "image", r.Image)
		if err != nil || im.ID != r.Image {
			return fmt.Errorf("service %s: captured rollback image unavailable; no pull or rebuild attempted", name)
		}
		if err = imageVolumesAllowed(im, r.Config, true); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) executeRollback(ctx context.Context, r Request, old *journal, state *secrets.State) (resp Response, err error) {
	resp.Version = 1
	next, preview, err := rollbackPlan(r, old)
	resp.RollbackPreview = preview
	if err != nil {
		return resp, err
	}
	if next == nil {
		s := m.multiStatus(ctx, old, state, false, "")
		resp.Status = &s
		return resp, nil
	}
	if old.Operation == r.Operation || retainedOperation(old, r.Operation) || old.LastMaintenanceOperation == r.Operation || (old.Completed != nil && old.Completed.Operation == r.Operation) || (old.PreviousCompleted != nil && old.PreviousCompleted.Operation == r.Operation) {
		return resp, errors.New("operation ID already used")
	}
	used, e := filepath.Glob(filepath.Join(m.dir(r.App), r.Operation+".*"))
	if e != nil || len(used) != 0 {
		return resp, errors.New("operation ID already used by captured inputs or a run")
	}
	if err = m.rollbackInputs(ctx, next); err != nil {
		return resp, err
	}
	if err = m.verifyMultiHost(); err != nil {
		return resp, err
	}
	if err = m.reserveHostnames(next); err != nil {
		return resp, err
	}
	if err = m.verifyRoutes(ctx, old); err != nil {
		return resp, err
	}
	if !r.Apply {
		return resp, nil
	}
	if old.Phase != "ready" {
		if err = m.supersedeMulti(ctx, old); err != nil {
			return resp, err
		}
	}
	// An immutable receipt prevents reuse after this operation leaves release history.
	// Each selected service gets its own receipt even when source keys coincide.
	for name, target := range next.Rollback.Targets {
		sum := bundleDigest([]byte(name))[:32]
		if err = atomic(filepath.Join(m.dir(r.App), r.Operation+"."+sum+".image.iid"), []byte(target.Image+"\n"), 0600); err != nil {
			return resp, err
		}
	}
	if err = m.save(next); err != nil {
		return resp, err
	}
	defer func() {
		if err != nil {
			next.LastError = diagnostic(err.Error())
			err = errors.Join(err, m.save(next))
		}
		s := m.multiStatus(ctx, next, state, false, "")
		resp.Status = &s
	}()
	err = m.advanceMulti(ctx, next)
	return resp, err
}
