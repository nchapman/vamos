package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/secrets"
)

// SecretUpdate is a runtime-only v8 operation. Unlike source-keyed build results,
// images are captured per service: services sharing a Dockerfile may have been
// deployed independently and must retain their different effective images.
type secretUpdateCapture struct {
	Versions map[string]string
	Images   map[string]string
}

func validSecretNames(r Request) bool {
	if r.SecretName != "" {
		return secrets.ValidName(r.SecretName) && len(r.SecretNames) == 0
	}
	if len(r.SecretNames) < 2 || len(r.SecretNames) > secrets.MaxNames {
		return false
	}
	seen := map[string]bool{}
	for _, name := range r.SecretNames {
		if !secrets.ValidName(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// SecretsInput validates the private payload separately from value-free request
// metadata. Singleton framing remains compatible with older staging clients.
func SecretsInput(r Request, input []byte) (map[string][]byte, error) {
	if !validSecretNames(r) {
		return nil, errors.New("invalid secret names")
	}
	if r.SecretName != "" {
		if err := secrets.ValidateValue(input); err != nil {
			return nil, err
		}
		return map[string][]byte{r.SecretName: input}, nil
	}
	return secrets.DecodeBatch(input, r.SecretNames)
}

func requestedSecretVersions(r Request) map[string]string {
	if r.SecretName != "" {
		return map[string]string{r.SecretName: r.Operation}
	}
	out := map[string]string{}
	for _, name := range r.SecretNames {
		out[name] = secrets.VersionFor(r.Operation, name)
	}
	return out
}

var (
	errSecretSetIncomplete  = errors.New("secret application requires a completed deployment; inspect status and finish recovery first")
	errSecretSetProtected   = errors.New("a consumer is a manual service; stage instead, then use an explicit scoped deploy with --yes")
	errSecretSetLegacy      = errors.New("secret application requires a captured multi-service deployment; deploy with a current client first")
	errSecretSetUnavailable = errors.New("a secret consumer or its captured image is unavailable or has drifted; inspect service status before recovery")
)

func secretUpdatePlan(r Request, old *journal) (*journal, error) {
	if old == nil || old.Phase == "destroyed" {
		return nil, nil
	}
	if old.Phase != "ready" {
		return nil, errSecretSetIncomplete
	}
	if !isMulti(old) {
		return nil, errSecretSetLegacy
	}
	next := &journal{Version: 8, Status: Status{App: old.App, URL: old.URL, Operation: r.Operation, Phase: "building"}, HostID: old.HostID,
		Config: &config.App{Name: old.App, Services: map[string]config.Service{}}, ServiceStates: map[string]*serviceState{},
		Completed: old.Completed, PreviousCompleted: old.PreviousCompleted, Removed: maps.Clone(old.Removed),
		SecretUpdate: &secretUpdateCapture{Versions: requestedSecretVersions(r), Images: map[string]string{}}}
	next.HostIdentity = old.HostIdentity
	for name, prior := range old.ServiceStates {
		if prior.Current == nil || prior.Current.Config == nil || prior.CurrentRetired || prior.Unavailable {
			return nil, errSecretSetIncomplete
		}
		current := prior.Current
		cfg := *current.Config
		refs := maps.Clone(current.SecretRefs)
		affected := false
		for key, ref := range refs {
			if version, ok := next.SecretUpdate.Versions[ref.Name]; ok && ref.Version != version {
				refs[key] = secrets.Ref{Name: ref.Name, Version: version}
				affected = true
			}
		}
		if affected && cfg.Update == config.UpdateManual {
			return nil, errSecretSetProtected
		}
		next.Config.Services[name] = cfg
		next.ServiceStates[name] = &serviceState{Phase: "pending", Current: current, Previous: prior.Previous, SecretRefs: refs, Skipped: !affected,
			Incarnation: serviceIncarnation(old, name), VolumeID: prior.VolumeID, DataMayHaveChanged: prior.DataMayHaveChanged}
		if affected {
			next.Selected = append(next.Selected, name)
			next.SecretUpdate.Images[name] = current.Image
		}
	}
	if len(next.Selected) == 0 {
		return nil, nil
	}
	sort.Strings(next.Selected)
	next.ServiceOrder = serviceOrder(next)
	if err := validateMultiJournal(next, next.App); err != nil {
		return nil, err
	}
	return next, nil
}

func (m *Manager) secretUpdateImages(ctx context.Context, j *journal) error {
	for _, name := range j.Selected {
		image := j.SecretUpdate.Images[name]
		im, err := m.inspect(ctx, "image", image)
		if err != nil || im.ID != image {
			return errSecretSetUnavailable
		}
		cfg := j.Config.Services[name]
		if err := imageVolumesAllowed(im, &cfg, true); err != nil {
			return errSecretSetUnavailable
		}
	}
	return nil
}

func (m *Manager) executeSecrets(ctx context.Context, r Request, input []byte, old *journal, hostID string) (Response, error) {
	resp := Response{Version: 1}
	values, err := SecretsInput(r, input)
	if err != nil {
		return resp, err
	}
	var next *journal
	if r.Action == "secrets-set" {
		next, err = secretUpdatePlan(r, old)
		if err != nil {
			return resp, err
		}
		if next != nil {
			if old.Operation == r.Operation || retainedOperation(old, r.Operation) || old.LastMaintenanceOperation == r.Operation || (old.Completed != nil && old.Completed.Operation == r.Operation) || (old.PreviousCompleted != nil && old.PreviousCompleted.Operation == r.Operation) {
				return resp, secrets.ErrOperationConflict
			}
			used, e := filepath.Glob(filepath.Join(m.dir(r.App), r.Operation+".*"))
			if e != nil || len(used) != 0 {
				return resp, secrets.ErrOperationConflict
			}
			if err = m.verifyMultiHost(); err != nil {
				return resp, err
			}
			if err = m.reserveHostnames(ctx, next); err != nil {
				return resp, err
			}
			if err = m.verifyRoutes(ctx, old); err != nil {
				return resp, err
			}
			if err = m.secretUpdateImages(ctx, next); err != nil {
				return resp, err
			}
			// Setting a secret updates running consumers; it does not implicitly start
			// stopped dependencies or bless replaced/mismatched runtime identities.
			for _, name := range next.Selected {
				for _, ref := range runtimeRefs(next.ServiceStates[name].Current) {
					v, e := m.instanceInspect(ctx, next, ref, false, true)
					if e != nil || !runtimeRunning(v) {
						return resp, errSecretSetUnavailable
					}
				}
			}
		}
	}
	// Stage and set share exactly one atomic store commit for the entire batch.
	entries, err := m.secretStore().StageBatch(r.App, hostID, r.Operation, values)
	if err != nil {
		return resp, err
	}
	if r.SecretName != "" {
		resp.Secret = &entries[0]
	} else {
		resp.SecretVersions = entries
	}
	if r.Action == "secrets-stage" {
		return resp, nil
	}
	if next == nil {
		resp.SetApplied = true
		return resp, nil
	}
	state, err := m.secretStore().Load(r.App, hostID)
	if err != nil {
		return resp, err
	}
	// No runtime transition is possible until all selected values and immutable
	// image identities have been captured durably. Resume never consults latest.
	for _, name := range next.Selected {
		if _, err = state.Values(next.ServiceStates[name].SecretRefs); err != nil {
			return resp, err
		}
		sum := bundleDigest([]byte(name))[:32]
		if err = atomic(filepath.Join(m.dir(r.App), r.Operation+"."+sum+".image.iid"), []byte(next.SecretUpdate.Images[name]+"\n"), 0600); err != nil {
			return resp, err
		}
	}
	if err = m.save(next); err != nil {
		return resp, err
	}
	resumed := r
	resumed.Action = "resume"
	resumed.SecretName = ""
	resumed.SecretNames = nil
	result, err := m.executeMulti(ctx, resumed, nil, next, nil, state)
	resp.Status = result.Status
	resp.SetApplied = err == nil
	return resp, err
}

func validateSecretUpdate(j *journal, settled bool) error {
	u := j.SecretUpdate
	bad := errors.New("invalid captured secret update")
	if u == nil || j.Version != 8 || j.Rollback != nil || j.Hook != nil || j.Config.Release != nil || j.Archive != "" || j.BundleSHA != "" || len(j.BuildImages) != 0 || j.Scope != "" || len(u.Versions) == 0 || len(u.Versions) > secrets.MaxNames || len(u.Images) != len(j.Selected) || (len(u.Images) == 0 && !settled) {
		return bad
	}
	for name, version := range u.Versions {
		if !secrets.ValidName(name) || !secrets.ValidVersion(version) {
			return bad
		}
	}
	for name, image := range u.Images {
		slot := j.ServiceStates[name]
		cfg, ok := j.Config.Services[name]
		if !ok || slot == nil || slot.Skipped || slot.New || cfg.Update == config.UpdateManual || !imageID.MatchString(image) || !slices.Contains(j.Selected, name) {
			return bad
		}
		affected := false
		for _, ref := range slot.SecretRefs {
			if version, ok := u.Versions[ref.Name]; ok {
				if version != ref.Version {
					return bad
				}
				affected = true
			}
		}
		if !affected {
			return bad
		}
	}
	for name, slot := range j.ServiceStates {
		if slot == nil || slot.Skipped != (u.Images[name] == "") {
			return bad
		}
	}
	return nil
}

func (m *Manager) secretUpdateInputs(ctx context.Context, j *journal) error {
	if err := m.secretUpdateImages(ctx, j); err != nil {
		return err
	}
	for _, name := range j.Selected {
		if _, err := m.secretValues(j, j.ServiceStates[name].SecretRefs); err != nil {
			return fmt.Errorf("service %s: captured secret version unavailable", name)
		}
	}
	return nil
}
