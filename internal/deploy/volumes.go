package deploy

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/volumes"
)

func (m *Manager) volumeStore() volumes.Store {
	return volumes.Store{Root: filepath.Join(m.Root, "var/lib/vamos/volumes")}
}

// Planning allocates intent only. The caller makes the archive durable before saving it.
func (m *Manager) planVolumes(r Request, old, next *journal) (*volumes.State, error) {
	s, err := m.volumeStore().Load(next.App, next.HostID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		s = &volumes.State{Version: 1, App: next.App, HostID: next.HostID, Records: map[string]volumes.Record{}}
	}
	if supportsVolumes(old) {
		if next.Version < min(old.Version, 4) {
			next.Version = min(old.Version, 4)
		}
		for name, slot := range old.ServiceStates {
			if slot != nil && slot.VolumeID != "" && s.Records[name].ID != slot.VolumeID {
				return nil, errors.New("missing or inconsistent volume registry")
			}
		}
		for _, rel := range retainedReleases(old) {
			if rel.VolumeID != "" && s.Records[rel.Service].ID != rel.VolumeID {
				return nil, errors.New("missing retained volume provenance")
			}
		}
	}
	for name, cfg := range next.Config.Services {
		rec, exists := s.Records[name]
		if cfg.Volume == nil {
			if exists {
				return nil, fmt.Errorf("service %s has an immutable volume binding", name)
			}
			continue
		}
		if next.Version < 3 {
			next.Version = 3
		}
		if old != nil && old.Phase != "destroyed" && old.Config != nil {
			if prev, ok := old.Config.Services[name]; ok && prev.Volume == nil {
				return nil, errors.New("cannot convert stateless service to volume-backed")
			}
		}
		if exists {
			if rec.Service != name || rec.Name != cfg.Volume.Name || rec.Mount != cfg.Volume.Mount {
				return nil, errors.New("immutable volume binding differs")
			}
			absent := old == nil || old.Phase == "destroyed" || old.ServiceStates[name] == nil
			if absent && old != nil && old.Phase != "destroyed" && !r.ReAdd {
				return nil, errors.New("retained service data requires re-add authorization")
			}
			if absent && !r.ReuseData {
				return nil, errors.New("retained volume requires reuse-data authorization")
			}
		} else {
			var id [16]byte
			if _, err = rand.Read(id[:]); err != nil {
				return nil, err
			}
			rec = volumes.Record{ID: hex.EncodeToString(id[:]), Service: name, Name: cfg.Volume.Name, Mount: cfg.Volume.Mount, Operation: next.Operation, State: "planned"}
			s.Records[name] = rec
		}
		if next.ServiceStates[name] == nil {
			return nil, errors.New("missing service volume slot")
		}
		next.ServiceStates[name].VolumeID = rec.ID
	}
	if len(s.Records) == 0 {
		return nil, nil
	}
	next.ReuseData = supportsVolumes(next) && (r.ReuseData || (old != nil && old.Phase != "destroyed" && old.ReuseData))
	if err = volumes.Validate(s); err != nil {
		return nil, err
	}
	return s, nil
}

func volumeMountArgument(name, mount string) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"type=volume", "source=" + name, "target=" + mount, "volume-nocopy"})
	w.Flush()
	return strings.TrimSuffix(b.String(), "\n")
}

func volumeLabels(j *journal, r volumes.Record) map[string]string {
	return map[string]string{"vamos.host": j.HostID, "vamos.app": j.App, "vamos.service": r.Service, "vamos.volume": r.ID, "vamos.operation": r.Operation}
}
func (m *Manager) inspectVolume(ctx context.Context, j *journal, r volumes.Record) (string, bool, error) {
	yes, err := m.exists(ctx, "volume", r.DockerName())
	if err != nil || !yes {
		return "", yes, err
	}
	b, err := m.cmd(ctx, "docker", "volume", "inspect", r.DockerName())
	if err != nil {
		return "", true, err
	}
	if len(b) > 64<<10 {
		return "", true, errors.New("volume inspection exceeds limit")
	}
	var rows []struct {
		Name, Driver, Scope, CreatedAt string
		Options, Labels                map[string]string
	}
	if json.Unmarshal(b, &rows) != nil || len(rows) != 1 {
		return "", true, errors.New("invalid volume inspection")
	}
	v := rows[0]
	_, err = time.Parse(time.RFC3339, v.CreatedAt)
	if err != nil || v.Name != r.DockerName() || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 || !reflect.DeepEqual(v.Labels, volumeLabels(j, r)) || (r.State == "created" && v.CreatedAt != r.CreatedAt) {
		return "", true, errors.New("foreign or replaced volume")
	}
	return v.CreatedAt, true, nil
}
func desiredVolume(s *volumes.State, j *journal, name string) (volumes.Record, error) {
	if s == nil || !supportsVolumes(j) || j.Config == nil {
		return volumes.Record{}, errors.New("missing volume provenance")
	}
	cfg, ok := j.Config.Services[name]
	slot := j.ServiceStates[name]
	r, found := s.Records[name]
	if !ok || cfg.Volume == nil || slot == nil || !found || slot.VolumeID != r.ID || cfg.Volume.Name != r.Name || cfg.Volume.Mount != r.Mount {
		return r, errors.New("volume reference differs from registry")
	}
	return r, nil
}
func (m *Manager) ensureVolumes(ctx context.Context, j *journal) error {
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil {
		return err
	}
	names := []string{}
	for name, cfg := range j.Config.Services {
		if cfg.Volume != nil && !(j.Scope != "" && j.ServiceStates[name].Skipped) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	missing := map[string]bool{}
	times := map[string]string{}
	// Complete all ownership and writer checks before creating anything.
	for _, name := range names {
		r, e := desiredVolume(s, j, name)
		if e != nil {
			return e
		}
		stamp, yes, e := m.inspectVolume(ctx, j, r)
		if e != nil {
			return e
		}
		if !yes && r.State == "created" {
			return errors.New("created volume is missing")
		}
		missing[name] = !yes
		times[name] = stamp
		if e = m.volumeReferences(ctx, j, r, "", true); e != nil {
			return e
		}
	}
	for _, name := range names {
		r := s.Records[name]
		if missing[name] {
			args := []string{"volume", "create", "--driver", "local"}
			labels := volumeLabels(j, r)
			for _, key := range []string{"vamos.host", "vamos.app", "vamos.service", "vamos.volume", "vamos.operation"} {
				args = append(args, "--label", key+"="+labels[key])
			}
			args = append(args, r.DockerName())
			_, createErr := m.cmd(ctx, "docker", args...)
			stamp, yes, e := m.inspectVolume(ctx, j, r)
			if e != nil {
				return e
			}
			if !yes {
				if createErr != nil {
					return createErr
				}
				return errors.New("created volume missing")
			}
			times[name] = stamp
		}
		if r.State == "planned" {
			r.State = "created"
			r.CreatedAt = times[name]
			s.Records[name] = r
			if err = m.volumeStore().Save(s); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateVolumeMounts(v *inspection, r volumes.Record) error {
	if len(v.Mounts) != 1 || len(v.HostConfig.Mounts) != 1 || len(v.HostConfig.Binds) != 0 || len(v.HostConfig.VolumesFrom) != 0 {
		return errors.New("unexpected runtime mounts")
	}
	var mount struct {
		Type, Name, Destination, Driver string
		RW                              bool
		SubPath                         string
	}
	if json.Unmarshal(v.Mounts[0], &mount) != nil || mount.Type != "volume" || mount.Name != r.DockerName() || mount.Destination != r.Mount || mount.Driver != "local" || !mount.RW || mount.SubPath != "" {
		return errors.New("runtime volume mount differs")
	}
	var spec struct {
		Type, Source, Target string
		ReadOnly             bool
		VolumeOptions        struct {
			NoCopy       bool
			Subpath      string
			Labels       map[string]string
			DriverConfig *struct {
				Name    string
				Options map[string]string
			}
		}
		BindOptions, TmpfsOptions json.RawMessage
		Consistency               string
	}
	if json.Unmarshal(v.HostConfig.Mounts[0], &spec) != nil || spec.Type != "volume" || spec.Source != r.DockerName() || spec.Target != r.Mount || spec.ReadOnly || !spec.VolumeOptions.NoCopy || spec.VolumeOptions.Subpath != "" || len(spec.VolumeOptions.Labels) != 0 || (spec.VolumeOptions.DriverConfig != nil && (spec.VolumeOptions.DriverConfig.Name != "" || len(spec.VolumeOptions.DriverConfig.Options) != 0)) || len(spec.BindOptions) != 0 || len(spec.TmpfsOptions) != 0 || spec.Consistency != "" {
		return errors.New("unsafe configured volume mount")
	}
	return nil
}
func (m *Manager) verifyRuntimeMounts(ctx context.Context, j *journal, r *release, v *inspection) error {
	if r == nil || r.Config == nil {
		return errors.New("missing runtime configuration")
	}
	if r.Config.Volume == nil {
		if len(v.Mounts) != 0 || len(v.HostConfig.Mounts) != 0 || len(v.HostConfig.Binds) != 0 || len(v.HostConfig.VolumesFrom) != 0 || r.VolumeID != "" {
			return errors.New("unexpected stateless mounts")
		}
		return nil
	}
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil {
		return err
	}
	if s == nil || !supportsVolumes(j) {
		return errors.New("missing volume registry")
	}
	rec, ok := s.Records[r.Service]
	if !ok || rec.ID != r.VolumeID || rec.Name != r.Config.Volume.Name || rec.Mount != r.Config.Volume.Mount || rec.State != "created" {
		return errors.New("runtime volume provenance differs")
	}
	if _, yes, e := m.inspectVolume(ctx, j, rec); e != nil {
		return e
	} else if !yes {
		return errors.New("runtime volume missing")
	}
	return validateVolumeMounts(v, rec)
}

// Unknown references are never modified, including stopped foreign containers.
func (m *Manager) volumeReferences(ctx context.Context, j *journal, rec volumes.Record, allow string, preflight bool) error {
	b, err := m.cmd(ctx, "docker", "container", "ls", "--all", "--filter", "volume="+rec.DockerName(), "--format", "{{json .}}")
	if err != nil {
		return err
	}
	if len(b) > 64<<10 {
		return errors.New("volume references exceed limit")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var row struct{ Names string }
		if json.Unmarshal([]byte(line), &row) != nil || row.Names == "" || seen[row.Names] {
			return errors.New("invalid volume references")
		}
		seen[row.Names] = true
		v, e := m.inspect(ctx, "container", row.Names)
		if e != nil {
			return e
		}
		op := v.Config.Labels["vamos.operation"]
		if !owned(v.Config.Labels, j, op) || !operation.MatchString(op) || v.Config.Labels["vamos.service"] != rec.Service || !maintenanceContainerIdentity(j, row.Names, rec.Service, op, replicaLabel(v.Config.Labels)) || strings.TrimPrefix(v.Name, "/") != row.Names {
			return errors.New("foreign volume reference")
		}
		if e = validateVolumeMounts(v, rec); e != nil {
			return e
		}
		if e = m.volumeContainerProvenance(j, rec, v); e != nil {
			return e
		}
		permitted := false
		if row.Names == allow {
			for _, rel := range retainedReleases(j) {
				if releaseHas(rel, allow) && runtimeLabelMatches(rel, allow, v.Config.Labels) && rel.Service == rec.Service && rel.VolumeID == rec.ID && rel.Operation == op && rel.Image == v.Config.Image {
					permitted = true
				}
			}
			if !permitted {
				return errors.New("writer is not the captured release")
			}
		}
		if preflight {
			if slot := j.ServiceStates[rec.Service]; slot != nil {
				for _, r := range []*release{slot.Current, slot.Candidate} {
					if releaseHas(r, row.Names) && runtimeLabelMatches(r, row.Names, v.Config.Labels) && r.VolumeID == rec.ID && r.Operation == op && r.Image == v.Config.Image {
						permitted = true
					}
				}
			}
		}
		if !permitted && (v.State.Running || v.State.Restarting || v.State.Paused || v.HostConfig.RestartPolicy.Name != "no") {
			return errors.New("another volume writer is active or restart-enabled")
		}
	}
	return nil
}

// Old cleanup candidates can outlive both snapshots. Their retained IID files
// are still provenance; matching Docker labels alone never authorize a reference.
func (m *Manager) volumeContainerProvenance(j *journal, rec volumes.Record, v *inspection) error {
	name, op := strings.TrimPrefix(v.Name, "/"), v.Config.Labels["vamos.operation"]
	for _, rel := range retainedReleases(j) {
		if releaseHas(rel, name) && runtimeLabelMatches(rel, name, v.Config.Labels) && rel.Service == rec.Service && rel.Operation == op && rel.VolumeID == rec.ID && rel.Image == v.Config.Image {
			return nil
		}
	}
	entries, err := os.ReadDir(m.dir(j.App))
	if err != nil {
		return errors.New("volume reference lacks captured provenance")
	}
	for _, entry := range entries {
		match := resourceFile.FindStringSubmatch(entry.Name())
		if match == nil || match[1] != op || !strings.HasSuffix(match[2], "image.iid") {
			continue
		}
		b, _, err := resourceRead(filepath.Join(m.dir(j.App), entry.Name()), 128)
		if err != nil {
			return err
		}
		if imageID.MatchString(v.Config.Image) && strings.TrimSpace(string(b)) == v.Config.Image {
			return nil
		}
	}
	return errors.New("volume reference lacks captured image provenance")
}

func (m *Manager) checkVolumeWriters(ctx context.Context, j *journal, r *release) error {
	if r.Config == nil || r.Config.Volume == nil {
		return nil
	}
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("missing volume registry")
	}
	rec, ok := s.Records[r.Service]
	if !ok || rec.ID != r.VolumeID || rec.Name != r.Config.Volume.Name || rec.Mount != r.Config.Volume.Mount || rec.State != "created" {
		return errors.New("writer volume reference differs")
	}
	if _, yes, e := m.inspectVolume(ctx, j, rec); e != nil {
		return e
	} else if !yes {
		return errors.New("writer volume missing")
	}
	ref, err := selectRuntime(r, 0)
	if err != nil {
		return err
	}
	return m.volumeReferences(ctx, j, rec, ref.Container, false)
}
func (m *Manager) volumeStatuses(ctx context.Context, app, hostID string, j *journal, service string) ([]VolumeStatus, error) {
	s, err := m.volumeStore().Load(app, hostID)
	if err != nil || s == nil {
		return nil, err
	}
	names := []string{}
	for name := range s.Records {
		if service == "" || name == service {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := []VolumeStatus{}
	identity := &journal{HostID: hostID, Status: Status{App: app}}
	for _, name := range names {
		r := s.Records[name]
		_, yes, e := m.inspectVolume(ctx, identity, r)
		if e != nil {
			return nil, e
		}
		v := VolumeStatus{Service: name, Name: r.Name, Mount: r.Mount, ID: r.ID, State: r.State, Missing: !yes}
		bound := false
		if j != nil {
			if slot := j.ServiceStates[name]; slot != nil && slot.VolumeID == r.ID {
				bound = j.Phase != "destroyed"
				v.DataMayHaveChanged = slot.DataMayHaveChanged
			}
		}
		if r.State == "created" && !bound {
			v.State = "retained"
			// A missing/destroyed deployment journal cannot prove retained data
			// was never executed against. Never advertise it as pristine.
			v.DataMayHaveChanged = true
		}
		out = append(out, v)
	}
	return out, nil
}
