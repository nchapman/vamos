package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/secrets"
)

func bundleDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Vamos's supported runtime has Docker-managed filtering and ordinary bridge NAT.
// Never silently repair daemon policy or restart Docker during app deployment.
func (m *Manager) verifyMultiHost() error {
	p := m.path(lifecycle.DockerDaemonPath)
	if err := safe(p); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return errors.New("unsupported Docker daemon configuration")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !sameJSON(data, []byte(lifecycle.DockerDaemonConfig)) {
		return errors.New("Docker daemon configuration drift; multi-service deployment requires the Vamos-managed networking policy")
	}
	return nil
}

func multiPlan(r Request, h *lifecycle.State, old *journal, state *secrets.State, data []byte, selection ...[]string) (*journal, error) {
	j := &journal{Version: 2, Status: Status{App: r.App, URL: "https://" + r.App + "." + r.Host.Domain, Phase: "building", Operation: r.Operation}, HostID: h.ID, Config: r.Config, Archive: r.Operation + ".context.tar", BundleSHA: bundleDigest(data), BuildImages: map[string]string{}, ServiceStates: map[string]*serviceState{}}
	j.Scope = r.Service
	if r.Partial {
		j.Selected = selectedServices(r)
		if len(selection) != 0 {
			j.Selected = selection[0]
		}
	}
	keepRequest := r
	if r.Partial {
		keepRequest.Config = bundleConfig(&journal{Version: 7, Config: r.Config, Selected: j.Selected, Status: Status{App: r.App}})
	}

	if r.Service != "" || workflowConfig(r.Config) || (old != nil && old.Version >= 4) {
		j.Version = 4
	}
	if domainConfig(r.Config) || (old != nil && old.Version == 6) {
		j.Version = 6
	}
	previousSlots := map[string]*serviceState{}
	if old != nil && old.Phase != "destroyed" {
		if isMulti(old) {
			previousSlots = old.ServiceStates
			j.Completed = old.Completed
			j.PreviousCompleted = old.PreviousCompleted
		} else {
			current := *old.Current
			cfg := old.Config.Services["web"]
			current.Config = &cfg
			previousSlots["web"] = &serviceState{Current: &current}
			j.Completed = &completedRelease{Operation: old.Operation, Services: map[string]*release{"web": &current}}
			if old.Previous != nil {
				j.PreviousCompleted = &completedRelease{Operation: old.Previous.Operation, Services: map[string]*release{"web": old.Previous}}
			}
		}
	}
	for name, cfg := range r.Config.Services {
		s := &serviceState{Phase: "pending"}
		if prev := previousSlots[name]; prev != nil {
			s.Current = prev.Current
			s.Previous = prev.Previous
			s.CurrentRetired = prev.CurrentRetired
			s.VolumeID = prev.VolumeID
			s.Unavailable = prev.Unavailable
			s.DataMayHaveChanged = prev.DataMayHaveChanged
		}
		keep, err := keepService(keepRequest, name, cfg, s.Current, state)
		if err != nil {
			return nil, err
		}
		s.Skipped = keep
		if keep {
			if s.Current == nil || s.CurrentRetired || s.Unavailable {
				return nil, fmt.Errorf("service %s: protected service requires explicit scoped recovery", name)
			}
			s.SecretRefs = maps.Clone(s.Current.SecretRefs)
		} else {
			s.SecretRefs, err = state.Snapshot(cfg.Secrets)
			if err != nil {
				return nil, fmt.Errorf("service %s: %w", name, err)
			}
		}
		s.New = s.Current == nil
		j.ServiceStates[name] = s
	}
	if err := prepareServiceIdentities(r, old, j); err != nil {
		return nil, err
	}
	j.ServiceOrder = serviceOrder(j)
	if err := captureRelease(j, r.Config.Release); err != nil {
		return nil, err
	}
	return j, nil
}

func (m *Manager) executeMulti(ctx context.Context, r Request, data []byte, j *journal, h *lifecycle.State, state *secrets.State) (resp Response, err error) {
	resp.Version = 1
	if r.Action == "deploy" {
		if r.ContextFormat != BundleFormat {
			return resp, errors.New("this app requires a current client with a captured multi-service build bundle")
		}
		selected := selectedServices(r)
		if supportsServiceIdentity(j) && !r.Partial {
			return resp, errors.New("partial deployment required for version 7 app")
		}
		if r.Supersede != "" && j != nil && (j.Scope != r.Service || !slices.Equal(capturedSelection(j), selected)) {
			return resp, errors.New("supersede must preserve the original service scope; resume instead")
		}
		if r.Config, err = effectiveConfig(r, j); err != nil {
			return
		}
		captured := &journal{Config: r.Config, Scope: r.Service, Status: Status{App: r.App}}
		if r.Partial {
			captured.Version, captured.Selected = 7, selected
		}
		if _, err = buildcontext.ReadBundle(data, bundleConfig(captured)); err != nil {
			return
		}

		if err = checkTransitions(j, r.Config); err != nil {
			return
		}
		if j == nil {
			if _, e := os.Lstat(m.dir(r.App)); !errors.Is(e, os.ErrNotExist) {
				return resp, errors.New("unjournaled app directory; manual recovery required")
			}
		} else {
			if j.Operation == r.Operation || retainedOperation(j, r.Operation) || j.LastMaintenanceOperation == r.Operation || (j.Completed != nil && j.Completed.Operation == r.Operation) || (j.PreviousCompleted != nil && j.PreviousCompleted.Operation == r.Operation) {
				return resp, errors.New("operation ID already used")
			}
			if j.Phase != "ready" && j.Phase != "destroyed" && r.Supersede == "" {
				return resp, errors.New("incomplete operation requires explicit resume or supersede")
			}
		}
		if _, e := os.Lstat(filepath.Join(m.dir(r.App), r.Operation+".context.tar")); !errors.Is(e, os.ErrNotExist) {
			return resp, errors.New("operation archive already exists or cannot be inspected")
		}
		used, scanErr := filepath.Glob(filepath.Join(m.dir(r.App), r.Operation+".*"))
		if scanErr != nil || len(used) != 0 {
			return resp, errors.New("operation ID already used by captured inputs or a run")
		}
		var next *journal
		next, err = multiPlan(r, h, j, state, data, selected)
		if err != nil {
			return
		}
		resp.DeployPreview = deploymentPreview(r, j)
		if resp.DeployPreview != nil && resp.DeployPreview.NeedsConfirmation {
			return resp, nil
		}
		if r.PreviewOnly {
			return resp, errors.New("selected service has no protected update to preview; no deployment started")
		}
		volumeState, volumeErr := m.planVolumes(r, j, next)
		if volumeErr != nil {
			return resp, volumeErr
		}
		if err = validateMultiJournal(next, r.App); err != nil {
			return
		}
		if err = m.reservePorts(ctx, next); err != nil {
			return
		}
		if err = m.reserveHostnames(ctx, next); err != nil {
			return
		}
		if err = m.verifyMultiHost(); err != nil {
			return
		}
		if r.Supersede != "" {
			if j == nil || !isMulti(j) || j.Operation != r.Supersede || (j.Phase != "building" && j.Phase != "releasing" && j.Phase != "updating") {
				return resp, errors.New("supersede does not match an incomplete multi-service operation")
			}
			if err = m.supersedeMulti(ctx, j); err != nil {
				return
			}
		} else if err = m.verifyRoutes(ctx, next); err != nil {
			return
		}
		if err = atomic(filepath.Join(m.dir(r.App), next.Archive), data, 0600); err != nil {
			return
		}
		if volumeState != nil {
			if err = m.volumeStore().Save(volumeState); err != nil {
				return
			}
		}
		if err = m.savePortClaims(ctx, next); err != nil {
			return
		}
		j = next
	} else if r.Action != "resume" || j == nil || !isMulti(j) || j.Operation != r.Operation {
		return resp, errors.New("resume operation does not match")
	}
	defer func() {
		if err != nil {
			j.LastError = diagnostic(err.Error())
			err = errors.Join(err, m.save(j))
		}
		s := m.multiStatus(ctx, j, state, false, "")
		resp.Status = &s
	}()
	// Reconcile write-before-reload before checks that could prevent further work.
	if err = m.reconcilePending(ctx, j); err != nil {
		return
	}
	if j.Phase != "ready" {
		if err = m.verifyMultiHost(); err != nil {
			return
		}
	}
	err = m.advanceMulti(ctx, j)
	return
}

func (m *Manager) supersedeMulti(ctx context.Context, j *journal) error {
	if slot := j.ServiceStates[j.ActiveService]; j.NewCaddy != "" && slot != nil && slot.Phase == "withdrawing" {
		return errors.New("public withdrawal is pending; resume before superseding")
	}
	for _, s := range j.ServiceStates {
		if s.Phase == "draining" {
			return errors.New("public retirement is pending; resume before superseding")
		}
	}
	if j.NewCaddy != "" {
		if err := m.verifyUnservedPromotion(ctx, j); err != nil {
			return err
		}
		s := j.ServiceStates[j.ActiveService]
		if err := m.multiDiscard(ctx, j, s.Candidate, false); err != nil {
			return err
		}
		if err := m.recordCancelledPromotion(j); err != nil {
			return err
		}
	}
	if err := m.verifyRoutes(ctx, j); err != nil {
		return err
	}
	for _, name := range j.ServiceOrder {
		s := j.ServiceStates[name]
		if s.CurrentRetired {
			if err := m.multiRetire(ctx, j, s.Current); err != nil {
				return fmt.Errorf("service %s retirement: %w", name, err)
			}
		}
		if s.Candidate != nil {
			for _, ref := range runtimeRefs(s.Candidate) {
				if retainedByEffectiveOrSnapshot(j, ref.Container) {
					return errors.New("candidate aliases a retained effective version")
				}
			}
			if err := m.multiDiscard(ctx, j, s.Candidate, s.Phase == "aliasing"); err != nil {
				return fmt.Errorf("service %s candidate cleanup: %w", name, err)
			}
		}
	}
	return nil
}

func retainedByEffectiveOrSnapshot(j *journal, name string) bool {
	for _, s := range j.ServiceStates {
		if releaseHas(s.Current, name) || releaseHas(s.Retiring, name) {
			return true
		}
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil {
			for _, r := range snapshot.Services {
				if releaseHas(r, name) {
					return true
				}
			}
		}
	}
	return false
}

func (m *Manager) advanceMulti(ctx context.Context, j *journal) error {
	if j.Phase == "ready" {
		return nil
	}
	var err error
	var archives map[string][]byte
	if j.Rollback == nil && j.SecretUpdate == nil {
		data, e := readContextArchive(filepath.Join(m.dir(j.App), j.Archive))
		if e != nil {
			return e
		}
		if bundleDigest(data) != j.BundleSHA {
			return errors.New("captured build bundle checksum mismatch")
		}
		archives, err = buildcontext.ReadBundle(data, bundleConfig(j))
		if err != nil {
			return err
		}
	}
	for _, name := range j.ServiceOrder {
		if j.ServiceStates[name].Skipped {
			continue
		}
		if _, err = m.secretValues(j, j.ServiceStates[name].SecretRefs); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		// Admission must precede retirement, particularly for private services:
		// unsupported limits must not stop an otherwise usable old runtime.
		if err = m.requireResourceSupport(ctx, j.Config.Services[name].Resources); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	if j.SecretUpdate != nil {
		err = m.secretUpdateInputs(ctx, j)
	} else if j.Rollback != nil {
		err = m.rollbackInputs(ctx, j)
	} else {
		err = m.multiBuild(ctx, j, archives)
	}
	if err != nil {
		return err
	}
	if j.Hook != nil && (j.Phase == "building" || j.Phase == "releasing") {
		if j.Phase == "building" {
			j.Phase = "releasing"
			if err = m.save(j); err != nil {
				return err
			}
		}
		if err = m.advanceRelease(ctx, j); err != nil {
			return err
		}
	}
	if err = m.ensureVolumes(ctx, j); err != nil {
		return err
	}
	if j.Phase == "building" || j.Phase == "releasing" {
		// Commit planning as a whole so inspection failure leaves a retryable
		// building journal, rather than a mixture of pending and prepared slots.
		planned := make(map[string]*serviceState, len(j.ServiceStates))
		for _, name := range j.ServiceOrder {
			cp := *j.ServiceStates[name]
			s := &cp
			planned[name] = s
			cfg := j.Config.Services[name]
			if s.Skipped {
				s.Phase = "ready"
				continue
			}
			image := selectedImage(j, name)
			unchanged := s.Current != nil && s.Current.Service != "" && !s.CurrentRetired && s.Current.Image == image && reflect.DeepEqual(*s.Current.Config, cfg) && maps.Equal(s.Current.SecretRefs, s.SecretRefs)
			if unchanged {
				for _, ref := range runtimeRefs(s.Current) {
					v, e := m.instanceInspect(ctx, j, ref, false, true)
					if e != nil {
						return fmt.Errorf("service %s: %w", name, e)
					}
					unchanged = unchanged && runtimeRunning(v) && v.HostConfig.RestartPolicy.Name == "unless-stopped"
				}
			}
			if unchanged {
				s.Phase = "ready"
				continue
			}
			s.Candidate = &release{VolumeID: s.VolumeID, Service: name, Config: &cfg, Container: containerName(j.App, name, j.Operation), Image: image, Operation: j.Operation, SecretRefs: s.SecretRefs}
			// New v8 identities are indexed, including singleton groups.
			if j.Version == 8 {
				s.Candidate.Container = ""
				for ordinal := 1; ordinal <= cfg.ReplicaCount(); ordinal++ {
					s.Candidate.Instances = append(s.Candidate.Instances, releaseInstance{Ordinal: ordinal, Container: instanceName(j.App, name, j.Operation, ordinal)})
				}
			}
			s.Phase = "prepared"
		}
		j.ServiceStates = planned
		j.Phase = "updating"
		if err = m.save(j); err != nil {
			return err
		}
	}
	if err = m.ensureNetwork(ctx, j); err != nil {
		return err
	}
	for _, name := range j.ServiceOrder {
		s := j.ServiceStates[name]
		if s.Phase == "ready" {
			continue
		}
		j.ActiveService = name
		if err = m.save(j); err != nil {
			return err
		}
		if err = m.advanceService(ctx, j, name); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	// Earlier steps may have completed before a long build/recovery gap. Sample
	// selected versions before completing the operation. Skipped service health
	// is reported separately; it must not make an untouched manual service a
	// resumable update that can only be repaired by violating its protection.
	for _, name := range j.ServiceOrder {
		if j.ServiceStates[name].Skipped {
			continue
		}
		if err := m.groupRunning(ctx, j, j.ServiceStates[name].Current); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	recordServiceHistory(j)
	if !sameEffective(j) {
		snapshot := &completedRelease{Operation: j.Operation, Services: map[string]*release{}}
		for name, s := range j.ServiceStates {
			snapshot.Services[name] = s.Current
		}
		j.PreviousCompleted = j.Completed
		j.Completed = snapshot
	}
	j.Phase = "ready"
	j.ActiveService = ""
	j.LastError = ""
	return m.save(j)
}

func (m *Manager) advanceService(ctx context.Context, j *journal, name string) error {
	s := j.ServiceStates[name]
	public := j.Config.Services[name].HTTP != nil
	if s.Phase == "prepared" && s.stopFirst(*s.Candidate.Config) {
		if err := m.multiEnsureRuntime(ctx, j, s.Candidate, true); err != nil {
			return err
		}
		if public && !s.Unavailable {
			s.Phase = "withdrawing"
			if err := m.save(j); err != nil {
				return err
			}
		}
	}
	if s.Phase == "withdrawing" {
		if err := m.promote(ctx, j); err != nil {
			return err
		}
		s.Unavailable = true
		j.OldCaddy, j.NewCaddy = "", ""
		s.Phase = "prepared"
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "prepared" {
		s.Phase = "starting"
		if (!public || s.stopFirst(*s.Candidate.Config)) && s.Current != nil {
			s.CurrentRetired = true
			s.Phase = "retiring"
		}
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "retiring" {
		if err := m.multiRetire(ctx, j, s.Current); err != nil {
			return err
		}
		s.Phase = "starting"
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "starting" {
		// Prove the old private writer remains stopped on every resumed attempt.
		if (!public || s.stopFirst(*s.Candidate.Config)) && s.Current != nil {
			if err := m.multiRetire(ctx, j, s.Current); err != nil {
				return err
			}
		}
		if err := m.multiEnsureCandidate(ctx, j, s.Candidate); err != nil {
			return err
		}
		if !public {
			if err := m.multiObserve(ctx, j, s.Candidate); err != nil {
				return err
			}
			s.Current = s.Candidate
			s.Candidate = nil
			s.CurrentRetired = false
			s.Phase = "ready"
			return m.save(j)
		}
		if err := m.multiHealthy(ctx, j, s.Candidate); err != nil {
			return err
		}
		s.Phase = "aliasing"
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "aliasing" {
		if err := m.multiAdmitAlias(ctx, j, s.Candidate); err != nil {
			return err
		}
		s.Phase = "promoting"
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "promoting" {
		if err := m.promote(ctx, j); err != nil {
			return err
		}
		s.Retiring = s.Current
		s.CurrentRetired = false
		s.Unavailable = false
		s.Current = s.Candidate
		s.Candidate = nil
		s.Phase = "draining"
		j.OldCaddy = ""
		j.NewCaddy = ""
		if err := m.save(j); err != nil {
			return err
		}
	}
	if s.Phase == "draining" {
		if s.Retiring != nil {
			if err := wait(ctx, m.Drain); err != nil {
				return err
			}
			if err := m.multiRetire(ctx, j, s.Retiring); err != nil {
				return err
			}
		}
		s.Retiring = nil
		s.Phase = "ready"
		return m.save(j)
	}
	if s.Phase != "ready" {
		return errors.New("unsupported service transition")
	}
	return nil
}
