package deploy

import (
	"github.com/nchapman/soar/internal/secrets"
)

const MaxServices = 8
const BundleFormat = "bundle-v1"

type ServiceStatus struct {
	Unavailable        bool   `json:"unavailable,omitempty"`
	DataMayHaveChanged bool   `json:"data_may_have_changed,omitempty"`
	Kind               string `json:"kind"`
	Phase              string `json:"phase"`
	URL                string `json:"url,omitempty"`
	Operation          string `json:"operation,omitempty"`
	Container          string `json:"container,omitempty"`
	Candidate          string `json:"candidate,omitempty"`
	Image              string `json:"image,omitempty"`
	Port               string `json:"port,omitempty"`
	Running            bool   `json:"running"`
	Available          bool   `json:"available"`
}

// Each slot describes effective state, not just the last requested app config.
// Retirement intent survives stopped workers and must never resurrect them.
type serviceState struct {
	VolumeID                     string `json:",omitempty"`
	Unavailable                  bool   `json:",omitempty"`
	DataMayHaveChanged           bool   `json:",omitempty"`
	New                          bool   `json:",omitempty"`
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
	add(j.Current)
	add(j.Previous)
	add(j.Candidate)
	for _, s := range j.ServiceStates {
		if s != nil {
			add(s.Current)
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
		if r.Container == name {
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
		return "soar-" + app + "-" + op
	}
	return "soar-" + app + "-" + service + "-" + op
}
