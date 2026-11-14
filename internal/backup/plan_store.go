package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const maxPlanRecordBytes = 24 * 1024

var errPlanState = errors.New("backup plan state is unavailable, unsafe, or belongs to another host")

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// PlanRevision hashes canonical policy and resolved identities, excluding the
// revision itself and installation time. It is not a repository snapshot ID.
func PlanRevision(r PlanRecord) (string, error) {
	return digestJSON(struct {
		Version  int
		HostID   string
		Plan     Plan
		Bindings PlanBindings
	}{r.Version, r.HostID, r.Plan, r.Bindings})
}

// ValidatePlanRecord also validates preview records, whose UpdatedAt is empty.
// Persisted records additionally require a committed timestamp.
func ValidatePlanRecord(r PlanRecord) error {
	if r.Version != 1 || !ValidOperation(r.HostID) || !digestPattern.MatchString(r.Revision) {
		return errPlanState
	}
	normalized, err := NormalizePlan(r.Plan)
	if err != nil || !reflect.DeepEqual(normalized, r.Plan) {
		return errPlanState
	}
	if len(r.Bindings.Sources) != len(r.Plan.Sources) || r.Bindings.Stops == nil || r.Bindings.Repositories == nil {
		return errPlanState
	}
	stops := map[string]bool{}
	repositories := map[string]bool{}
	for _, set := range r.Plan.Sets {
		for _, name := range set.Stop {
			stops[name] = true
		}
		repositories[set.Repository] = true
	}
	if len(stops) != len(r.Bindings.Stops) || len(repositories) != len(r.Bindings.Repositories) {
		return errPlanState
	}
	for name := range stops {
		if !ValidOperation(r.Bindings.Stops[name]) {
			return errPlanState
		}
	}
	for name := range repositories {
		pin, ok := r.Bindings.Repositories[name]
		if !ok || !digestPattern.MatchString(pin.ID) || !digestPattern.MatchString(pin.ConfigDigest) {
			return errPlanState
		}
	}
	identities := map[string]string{}
	volumes := map[string]PlanVolume{}
	volumeOwners := map[string]string{}
	bindIdentity := func(name, id string) bool {
		if !ValidName(name) || !ValidOperation(id) {
			return false
		}
		if previous, ok := identities[name]; ok && previous != id {
			return false
		}
		identities[name] = id
		return true
	}
	for name, id := range r.Bindings.Stops {
		if !bindIdentity(name, id) {
			return errPlanState
		}
	}
	for name, source := range r.Plan.Sources {
		binding, ok := r.Bindings.Sources[name]
		if !ok || binding.Service != source.Service || !bindIdentity(binding.Service, binding.Incarnation) {
			return errPlanState
		}
		if source.Type == "postgres" {
			if binding.Volume != nil || binding.CredentialService != source.Credentials.Service || !bindIdentity(binding.CredentialService, binding.CredentialIncarnation) {
				return errPlanState
			}
		} else {
			volume := binding.Volume
			if volume == nil || !ValidOperation(volume.ID) || volume.Name != source.Volume || len(volume.Mount) > 4096 || !path.IsAbs(volume.Mount) || path.Clean(volume.Mount) != volume.Mount || volume.Mount == "/" || strings.ContainsAny(volume.Mount, "\x00\r\n\t") || binding.CredentialService != "" || binding.CredentialIncarnation != "" {
				return errPlanState
			}
			if previous, ok := volumes[binding.Service]; ok && previous != *volume {
				return errPlanState
			}
			if owner, ok := volumeOwners[volume.ID]; ok && owner != binding.Service {
				return errPlanState
			}
			volumes[binding.Service] = *volume
			volumeOwners[volume.ID] = binding.Service
		}
	}
	if len(identities) > 8 {
		return errPlanState
	}
	if r.UpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err != nil {
			return errPlanState
		}
	}
	digest, err := PlanRevision(r)
	if err != nil || digest != r.Revision {
		return errPlanState
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > maxPlanRecordBytes {
		return errPlanState
	}
	return nil
}

func (m *Manager) planDirectory() store {
	return store{dir: filepath.Join(m.Root, "var/lib/soar/backups/plans")}
}
func (m *Manager) loadPlan(app, hostID string) (*PlanRecord, error) {
	if !ValidName(app) || !ValidOperation(hostID) {
		return nil, errPlanState
	}
	directory := m.planDirectory()
	if err := directory.checkDir(false); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, errPlanState
	}
	filename := filepath.Join(directory.dir, app+".json")
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !private(info, false) || info.Size() > maxPlanRecordBytes {
		return nil, errPlanState
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, errPlanState
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !private(opened, false) || !os.SameFile(info, opened) {
		return nil, errPlanState
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPlanRecordBytes+1))
	if err != nil || len(data) > maxPlanRecordBytes {
		return nil, errPlanState
	}
	var record PlanRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.HostID != hostID || record.Plan.App != app || record.UpdatedAt == "" || ValidatePlanRecord(record) != nil {
		return nil, errPlanState
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, errPlanState
	}
	return &record, nil
}
func (m *Manager) savePlan(record PlanRecord, expected string) error {
	if record.UpdatedAt == "" || ValidatePlanRecord(record) != nil {
		return errPlanState
	}
	old, err := m.loadPlan(record.Plan.App, record.HostID)
	if err != nil {
		return err
	}
	revision := "none"
	if old != nil {
		revision = old.Revision
	}
	if revision != expected {
		return errPlanConflict
	}
	directory := m.planDirectory()
	if directory.checkDir(true) != nil {
		return errPlanState
	}
	// Compact private records keep both sides of a preview within the metadata
	// protocol. Application artifacts never use this path.
	data, err := json.Marshal(record)
	if err != nil || len(data)+1 > maxPlanRecordBytes {
		return errPlanState
	}
	f, err := os.CreateTemp(directory.dir, ".tmp-")
	if err != nil {
		return errPlanState
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errPlanState
	}
	if os.Rename(name, filepath.Join(directory.dir, record.Plan.App+".json")) != nil || syncDirectory(directory.dir) != nil {
		return errPlanState
	}
	return nil
}
