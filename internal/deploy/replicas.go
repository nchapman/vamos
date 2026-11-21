package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nchapman/vamos/internal/config"
)

// runtimeRef refers directly into the journal. In particular port intent edits
// must reach the persisted instance before Docker creation, not a release copy.
// Configuration, image and secrets are shared once by the whole release.
type runtimeRef struct {
	*release
	Container         string
	Port, PublishPort *string
	Ordinal           int // zero denotes a legacy scalar identity, never a replica label
}

func runtimeRefs(r *release) []runtimeRef {
	if r == nil {
		return nil
	}
	if len(r.Instances) == 0 {
		return []runtimeRef{{release: r, Container: r.Container, Port: &r.Port, PublishPort: &r.PublishPort}}
	}
	refs := make([]runtimeRef, len(r.Instances))
	for i := range r.Instances {
		v := &r.Instances[i]
		refs[i] = runtimeRef{release: r, Container: v.Container, Port: &v.Port, PublishPort: &v.PublishPort, Ordinal: v.Ordinal}
	}
	return refs
}

func releaseHas(r *release, name string) bool {
	if name == "" {
		return false
	}
	for _, ref := range runtimeRefs(r) {
		if ref.Container == name {
			return true
		}
	}
	return false
}

// Retained releases authorize only their exact recorded members. After a
// generation ages out of history, maintenance additionally requires its durable
// immutable-image receipt, alongside the exact host/app/service/op/ordinal name
// and labels checked by resourceContainer. Prefix matching is never authority.
func (m *Manager) replicaResourceProvenance(j *journal, v *resourceInspection, name string) error {
	if replicaLabel(v.Config.Labels) == 0 {
		return nil
	}
	op, service := v.Config.Labels["vamos.operation"], v.Config.Labels["vamos.service"]
	for _, r := range retainedReleases(j) {
		if r.Operation != op || r.Service != service {
			continue
		}
		if releaseHas(r, name) && runtimeLabelMatches(r, name, v.Config.Labels) && r.Image == v.Config.Image {
			return nil
		}
		return errors.New("maintenance replica is not a captured group member")
	}
	entries, err := os.ReadDir(m.dir(j.App))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		match := resourceFile.FindStringSubmatch(entry.Name())
		if match == nil || match[1] != op || !strings.HasSuffix(match[2], "image.iid") {
			continue
		}
		data, _, err := resourceRead(filepath.Join(m.dir(j.App), entry.Name()), 128)
		if err != nil {
			return err
		}
		if imageID.MatchString(v.Config.Image) && strings.TrimSpace(string(data)) == v.Config.Image {
			return nil
		}
	}
	return errors.New("maintenance replica lacks captured image provenance")
}

func replicaLabel(labels map[string]string) int {
	value, present := labels["vamos.replica"]
	if !present {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > config.MaxAppReplicas || strconv.Itoa(n) != value {
		return -1
	}
	return n
}

func runtimeLabelMatches(r *release, name string, labels map[string]string) bool {
	for _, ref := range runtimeRefs(r) {
		if ref.Container != name {
			continue
		}
		value, present := labels["vamos.replica"]
		if ref.Ordinal == 0 {
			return !present
		}
		return value == strconv.Itoa(ref.Ordinal)
	}
	return false
}

func releasePortsReady(r *release) bool {
	for _, ref := range runtimeRefs(r) {
		if !validPort(*ref.Port) {
			return false
		}
	}
	return r != nil
}

func instanceName(app, service, op string, ordinal int) string {
	name := containerName(app, service, op)
	if ordinal > 0 {
		name += "-" + strconv.Itoa(ordinal)
	}
	return name
}

func supportsServiceIdentity(j *journal) bool { return j != nil && (j.Version == 7 || j.Version == 8) }
func replicaConfig(a *config.App) bool {
	if a != nil {
		for _, s := range a.Services {
			if s.ReplicaCount() != 1 {
				return true
			}
		}
	}
	return false
}

func selectRuntime(r *release, ordinal int) (runtimeRef, error) {
	refs := runtimeRefs(r)
	if ordinal == 0 && len(refs) > 1 {
		return runtimeRef{}, errors.New("replica target is ambiguous; select an ordinal with --replica")
	}
	if ordinal == 0 {
		ordinal = 1
	}
	if ordinal < 1 || ordinal > len(refs) {
		return runtimeRef{}, errors.New("selected replica has no recorded container")
	}
	return refs[ordinal-1], nil
}

func validateInstances(j *journal, r *release) error {
	bad := errors.New("invalid release instance membership")
	if len(r.Instances) == 0 {
		if r.Config != nil && r.Config.ReplicaCount() != 1 {
			return bad
		}
		if r.Container != containerName(j.App, r.Service, r.Operation) {
			return bad
		}
		return nil
	}
	if j.Version != 8 || r.Service == "" || r.Config == nil || r.Container != "" || r.Port != "" || r.PublishPort != "" || len(r.Instances) != r.Config.ReplicaCount() || len(r.Instances) > config.MaxAppReplicas || (r.Config.StopFirst() && len(r.Instances) > 1) {
		return bad
	}
	ports := map[string]bool{}
	for i, v := range r.Instances {
		if v.Ordinal != i+1 || v.Container != instanceName(j.App, r.Service, r.Operation, i+1) {
			return bad
		}
		if v.PublishPort != "" {
			if ports[v.PublishPort] {
				return bad
			}
			ports[v.PublishPort] = true
		}
	}
	return nil
}

func (m *Manager) eachRuntime(ctx context.Context, j *journal, r *release, fn func(context.Context, *journal, runtimeRef) error) error {
	if r == nil {
		return errors.New("missing release")
	}
	if err := validateInstances(j, r); err != nil {
		return err
	}
	for _, ref := range runtimeRefs(r) {
		if err := fn(ctx, j, ref); err != nil {
			return fmt.Errorf("instance %d: %w", max(1, ref.Ordinal), err)
		}
	}
	return nil
}

// Group operations retain the existing service-level state machine. A partial
// failure leaves the same complete recorded set for the next attempt.
func (m *Manager) multiEnsureRuntime(c context.Context, j *journal, r *release, prepare bool) error {
	return m.eachRuntime(c, j, r, func(c context.Context, j *journal, v runtimeRef) error {
		return m.instanceEnsureRuntime(c, j, v, prepare)
	})
}
func (m *Manager) multiHealthy(c context.Context, j *journal, r *release) error {
	return m.eachRuntime(c, j, r, m.instanceHealthy)
}
func (m *Manager) multiObserve(c context.Context, j *journal, r *release) error {
	return m.eachRuntime(c, j, r, m.instanceObserve)
}
func (m *Manager) multiAdmitAlias(c context.Context, j *journal, r *release) error {
	return m.eachRuntime(c, j, r, m.instanceAdmitAlias)
}
func (m *Manager) multiStop(c context.Context, j *journal, r *release, detached, alias bool) error {
	return m.eachRuntime(c, j, r, func(c context.Context, j *journal, v runtimeRef) error {
		return m.instanceStop(c, j, v, detached, alias)
	})
}
func (m *Manager) multiDiscard(c context.Context, j *journal, r *release, detached bool) error {
	return m.eachRuntime(c, j, r, func(c context.Context, j *journal, v runtimeRef) error { return m.instanceDiscard(c, j, v, detached) })
}

// Scalar compatibility for legacy callers. Group callers must explicitly either
// inspect every member or select a member; returning one member would lie.
func (m *Manager) multiInspect(c context.Context, j *journal, r *release, detached, alias bool) (*inspection, error) {
	ref, err := selectRuntime(r, 0)
	if err != nil {
		return nil, err
	}
	return m.instanceInspect(c, j, ref, detached, alias)
}
func (m *Manager) groupRunning(c context.Context, j *journal, r *release) error {
	return m.eachRuntime(c, j, r, func(c context.Context, j *journal, v runtimeRef) error {
		observed, err := m.instanceInspect(c, j, v, false, true)
		if err != nil {
			return err
		}
		if !runtimeRunning(observed) || observed.HostConfig.RestartPolicy.Name != "unless-stopped" {
			return errors.New("effective version is not running")
		}
		return nil
	})
}
func (m *Manager) groupStopped(c context.Context, j *journal, r *release) error {
	return m.eachRuntime(c, j, r, func(c context.Context, j *journal, v runtimeRef) error {
		observed, err := m.instanceInspect(c, j, v, false, true)
		if err != nil {
			return err
		}
		if observed.State.Running || observed.State.Restarting || observed.State.Paused || observed.HostConfig.RestartPolicy.Name != "no" {
			return errors.New("private retirement not proven")
		}
		return nil
	})
}
