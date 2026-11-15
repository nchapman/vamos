package deploy

import (
	"github.com/nchapman/vamos/internal/secrets"
)

const MaxServices = 8
const BundleFormat = "bundle-v1"

type InstanceStatus struct {
	Ordinal   int    `json:"ordinal"`
	Container string `json:"container"`
	Port      string `json:"port,omitempty"`
	Running   bool   `json:"running"`
	Available bool   `json:"available"`
}

type ServiceStatus struct {
	DesiredInstances   int              `json:"desired_instances,omitempty"`
	CurrentInstances   int              `json:"current_instances,omitempty"`
	RunningInstances   int              `json:"running_instances"`
	AvailableInstances int              `json:"available_instances"`
	Instances          []InstanceStatus `json:"instances,omitempty"`
	CandidateInstances []InstanceStatus `json:"candidate_instances,omitempty"`
	Incarnation        string           `json:"incarnation,omitempty"`
	Domains            []DomainStatus   `json:"domains,omitempty"`
	Unavailable        bool             `json:"unavailable,omitempty"`
	DataMayHaveChanged bool             `json:"data_may_have_changed,omitempty"`
	Kind               string           `json:"kind"`
	Phase              string           `json:"phase"`
	URL                string           `json:"url,omitempty"`
	Operation          string           `json:"operation,omitempty"`
	Container          string           `json:"container,omitempty"`
	Candidate          string           `json:"candidate,omitempty"`
	Image              string           `json:"image,omitempty"`
	Port               string           `json:"port,omitempty"`
	Running            bool             `json:"running"`
	Available          bool             `json:"available"`
}

// Each slot describes effective state, not just the last requested app config.
// Retirement intent survives stopped workers and must never resurrect them.
type serviceState struct {
	Incarnation                  string   `json:",omitempty"`
	Previous                     *release `json:",omitempty"`
	Skipped                      bool     `json:",omitempty"`
	VolumeID                     string   `json:",omitempty"`
	Unavailable                  bool     `json:",omitempty"`
	DataMayHaveChanged           bool     `json:",omitempty"`
	New                          bool     `json:",omitempty"`
	Phase                        string
	SecretRefs                   map[string]secrets.Ref `json:",omitempty"`
	Current, Candidate, Retiring *release
	CurrentRetired               bool `json:",omitempty"`
}

type completedRelease struct {
	Operation string
	Services  map[string]*release
}

// Retention must traverse snapshots as well as the mixed effective state left
// by partially successful operations. Operation IDs are not container IDs.
func retainedReleases(j *journal) []*release {
	if j == nil {
		return nil
	}
	out := []*release{}
	add := func(r *release) {
		if r != nil {
			out = append(out, r)
		}
	}
	if j.Rollback != nil {
		for _, r := range j.Rollback.Targets {
			add(r)
		}
	}
	add(j.Current)
	add(j.Previous)
	add(j.Candidate)
	for _, s := range j.ServiceStates {
		if s != nil {
			add(s.Current)
			add(s.Previous)
			add(s.Candidate)
			add(s.Retiring)
		}
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil {
			for _, r := range snapshot.Services {
				add(r)
			}
		}
	}
	return out
}

func retainedOperation(j *journal, op string) bool {
	for _, r := range retainedReleases(j) {
		if r.Operation == op {
			return true
		}
	}
	return false
}

func retainedContainer(j *journal, name string) bool {
	for _, r := range retainedReleases(j) {
		if releaseHas(r, name) {
			return true
		}
	}
	return false
}

func hasCurrent(j *journal) bool {
	if j.Current != nil {
		return true
	}
	for _, s := range j.ServiceStates {
		if s != nil && s.Current != nil {
			return true
		}
	}
	return false
}

func containerName(app, service, op string) string {
	if service == "" {
		return "vamos-" + app + "-" + op
	}
	return "vamos-" + app + "-" + service + "-" + op
}
