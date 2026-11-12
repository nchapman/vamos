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

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/secrets"
)

func bundleDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Soar's supported runtime has Docker-managed filtering and ordinary bridge NAT.
// Never silently repair daemon policy or restart Docker during app deployment.
func (m *Manager) verifyMultiHost() error {
	p := m.path("/etc/docker/daemon.json")
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
	if !sameJSON(data, []byte(`{"bridge":"none","log-driver":"local"}`)) {
		return errors.New("Docker daemon configuration drift; multi-service deployment requires the Soar-managed networking policy")
	}
	return nil
}

func multiPlan(r Request, h *lifecycle.State, old *journal, state *secrets.State, data []byte) (*journal, error) {
	j := &journal{Version: 2, Status: Status{App: r.App, URL: "https://" + r.App + "." + r.Host.Domain, Phase: "building", Operation: r.Operation}, HostID: h.ID, Config: r.Config, Archive: r.Operation + ".context.tar", BundleSHA: bundleDigest(data), BuildImages: map[string]string{}, ServiceStates: map[string]*serviceState{}}
	previousSlots := map[string]*serviceState{}
	if old != nil && old.Phase != "destroyed" {
		if old.Version == 2 {
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
		refs, err := state.Snapshot(cfg.Secrets)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		s := &serviceState{Phase: "pending", SecretRefs: refs}
		if prev := previousSlots[name]; prev != nil {
			s.Current = prev.Current
			s.CurrentRetired = prev.CurrentRetired
		}
		s.New = s.Current == nil
		j.ServiceStates[name] = s
	}
	j.ServiceOrder = serviceOrder(j)
	return j, nil
}

func (m *Manager) executeMulti(ctx context.Context, r Request, data []byte, j *journal, h *lifecycle.State, state *secrets.State) (resp Response, err error) {
	resp.Version = 1
	if r.Action == "deploy" {
		if r.ContextFormat != BundleFormat {
			return resp, errors.New("this app requires a current client with a captured multi-service build bundle")
		}
		if _, err = buildcontext.ReadBundle(data, r.Config); err != nil {
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
		var next *journal
		next, err = multiPlan(r, h, j, state, data)
		if err != nil {
			return
		}
		if err = validateMultiJournal(next, r.App); err != nil {
			return
		}
		if err = m.reserveHostnames(next); err != nil {
			return
		}
		if err = m.verifyMultiHost(); err != nil {
			return
		}
		if r.Supersede != "" {
			if j == nil || j.Version != 2 || j.Operation != r.Supersede || (j.Phase != "building" && j.Phase != "updating") {
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
		if err = m.save(next); err != nil {
			return
		}
		j = next
	} else if r.Action != "resume" || j == nil || j.Version != 2 || j.Operation != r.Operation {
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
	if j.NewCaddy != "" {
		return errors.New("route promotion is unresolved; resume before superseding")
	}
	for _, s := range j.ServiceStates {
		if s.Phase == "draining" {
			return errors.New("public retirement is pending; resume before superseding")
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
			if retainedByEffectiveOrSnapshot(j, s.Candidate.Container) {
				return errors.New("candidate aliases a retained effective version")
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
		if (s.Current != nil && s.Current.Container == name) || (s.Retiring != nil && s.Retiring.Container == name) {
			return true
		}
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil {
			for _, r := range snapshot.Services {
				if r.Container == name {
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
	data, err := readContextArchive(filepath.Join(m.dir(j.App), j.Archive))
	if err != nil {
		return err
	}
	if bundleDigest(data) != j.BundleSHA {
		return errors.New("captured build bundle checksum mismatch")
	}
	archives, err := buildcontext.ReadBundle(data, j.Config)
	if err != nil {
		return err
	}
	for _, name := range j.ServiceOrder {
		if _, err = m.secretValues(j, j.ServiceStates[name].SecretRefs); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	if err = m.multiBuild(ctx, j, archives); err != nil {
		return err
	}
	if j.Phase == "building" {
		// Commit planning as a whole so inspection failure leaves a retryable
		// building journal, rather than a mixture of pending and prepared slots.
		planned := make(map[string]*serviceState, len(j.ServiceStates))
		for _, name := range j.ServiceOrder {
			copy := *j.ServiceStates[name]
			s := &copy
			planned[name] = s
			cfg := j.Config.Services[name]
			image := j.BuildImages[buildcontext.BuildKey(*cfg.Build)]
			unchanged := s.Current != nil && s.Current.Service != "" && !s.CurrentRetired && s.Current.Image == image && reflect.DeepEqual(*s.Current.Config, cfg) && maps.Equal(s.Current.SecretRefs, s.SecretRefs)
			if unchanged {
				v, e := m.multiInspect(ctx, j, s.Current, false, true)
				if e != nil {
					return fmt.Errorf("service %s: %w", name, e)
				}
				unchanged = v.State.Running && v.HostConfig.RestartPolicy.Name == "unless-stopped"
			}
			if unchanged {
				s.Phase = "ready"
				continue
			}
			s.Candidate = &release{Service: name, Config: &cfg, Container: containerName(j.App, name, j.Operation), Image: image, Operation: j.Operation, SecretRefs: s.SecretRefs}
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
	// Earlier service steps may have completed before a long build/recovery gap.
	// Sample every effective version before declaring the whole app complete.
	for _, name := range j.ServiceOrder {
		v, err := m.multiInspect(ctx, j, j.ServiceStates[name].Current, false, true)
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		if !runtimeRunning(v) || v.HostConfig.RestartPolicy.Name != "unless-stopped" {
			return fmt.Errorf("service %s: effective version is not running", name)
		}
	}
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
	if s.Phase == "prepared" {
		s.Phase = "starting"
		if !public && s.Current != nil {
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
		if !public && s.Current != nil {
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
