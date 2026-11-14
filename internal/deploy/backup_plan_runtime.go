package deploy

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nchapman/soar/internal/backup"
)

// backupPlanRuntime observes effective identities only. In particular it must
// not use preflightRuns: that mutation preflight can reconcile and save runs.
func (m *Manager) backupPlanRuntime(ctx context.Context, j *journal) (backup.PlanRuntime, error) {
	fail := func() (backup.PlanRuntime, error) {
		return backup.PlanRuntime{}, errBackupPlanRuntime
	}
	if j == nil || (j.Version != 7 && j.Version != 8) {
		return backup.PlanRuntime{}, errBackupPlanLegacy
	}
	if j.Phase != "ready" || j.Maintenance != nil || j.Rollback != nil || j.Candidate != nil || (j.Hook != nil && j.Hook.ExitCode == nil) || len(j.ServiceStates) == 0 {
		return fail()
	}
	paths, err := filepath.Glob(filepath.Join(m.dir(j.App), "*.run.json"))
	if err != nil || len(paths) > 256 {
		return fail()
	}
	for _, p := range paths {
		r, e := m.loadRun(j, strings.TrimSuffix(filepath.Base(p), ".run.json"))
		if e != nil || r.Phase != "complete" {
			return fail()
		}
		v, e := m.inspect(ctx, "container", r.Container)
		if e != nil {
			if m.runContainerAbsent(ctx, r.Container) {
				continue
			}
			return fail()
		}
		if v.State.Running || v.State.Restarting || v.State.Paused || m.verifyRun(ctx, j, r, v) != nil {
			return fail()
		}
	}
	out := backup.PlanRuntime{HostID: j.HostID, App: j.App, Services: map[string]backup.PlanService{}}
	for name, slot := range j.ServiceStates {
		if slot == nil || slot.Current == nil || slot.Current.Config == nil || slot.Candidate != nil || slot.Retiring != nil || slot.CurrentRetired || !operation.MatchString(slot.Incarnation) {
			return fail()
		}
		r := slot.Current
		if r.Service != name {
			return fail()
		}
		cfg := r.Config
		refs := runtimeRefs(r)
		if len(refs) != cfg.ReplicaCount() {
			return fail()
		}
		for _, ref := range refs {
			v, e := m.instanceInspect(ctx, j, ref, false, true)
			if e != nil || v.State.Restarting || v.State.Paused || v.State.Dead || v.HostConfig.RestartPolicy.Name != "unless-stopped" {
				return fail()
			}
		}
		s := backup.PlanService{Incarnation: slot.Incarnation, Replicas: cfg.ReplicaCount()}
		// instanceInspect has validated exact mappings and resolved the applied pins.
		for key := range cfg.Secrets {
			s.SecretEnvs = append(s.SecretEnvs, key)
		}
		sort.Strings(s.SecretEnvs)
		if cfg.Volume != nil {
			state, e := m.volumeStore().Load(j.App, j.HostID)
			if e != nil || state == nil {
				return fail()
			}
			rec, ok := state.Records[name]
			if !ok || rec.State != "created" || rec.ID != slot.VolumeID || rec.ID != r.VolumeID || rec.Name != cfg.Volume.Name || rec.Mount != cfg.Volume.Mount {
				return fail()
			}
			// instanceInspect already checked mounts and exact Docker creation identity.
			s.Volume = &backup.PlanVolume{ID: rec.ID, Name: rec.Name, Mount: rec.Mount}
		} else if slot.VolumeID != "" {
			return fail()
		}
		out.Services[name] = s
	}
	return out, nil
}
