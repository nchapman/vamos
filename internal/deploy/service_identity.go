package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"sort"
)

// Legacy identities are read-only compatibility tokens. The first v7 operation
// freezes them into the journal; a concurrent legacy replacement invalidates a
// previously observed token instead of silently authorizing a different release.
func serviceIncarnation(j *journal, name string) string {
	if j == nil || j.Phase == "destroyed" {
		return ""
	}
	var rel *release
	if j.Version == 1 {
		if name != "web" {
			return ""
		}
		rel = j.Current
		if rel == nil {
			rel = j.Candidate
		}
	} else {
		slot := j.ServiceStates[name]
		if slot == nil {
			return ""
		}
		if slot.Incarnation != "" {
			return slot.Incarnation
		}
		rel = slot.Current
		if rel == nil {
			rel = slot.Candidate
		}
	}
	origin := j.Operation
	if rel != nil {
		origin = rel.Operation + "/" + rel.Container
	}
	sum := sha256.Sum256([]byte(j.HostID + "/" + j.App + "/" + name + "/" + origin))
	return hex.EncodeToString(sum[:16])
}

// upgradeServiceJournal changes metadata only; callers must save it as part of
// their locked durable operation, never as a side effect of binding/status.
func upgradeServiceJournal(j *journal) {
	if j == nil || !isMulti(j) {
		return
	}
	for name, slot := range j.ServiceStates {
		if slot.Incarnation == "" {
			slot.Incarnation = serviceIncarnation(j, name)
		}
	}
	if len(j.Selected) == 0 {
		if j.Scope != "" {
			j.Selected = []string{j.Scope}
		} else {
			for name := range j.Config.Services {
				j.Selected = append(j.Selected, name)
			}
			sort.Strings(j.Selected)
		}
	}
	j.Version = 7
}

func prepareServiceIdentities(r Request, old, next *journal) error {
	if !r.Partial {
		if old != nil && old.Version == 7 {
			return errors.New("this app requires service bindings and a current partial-manifest client")
		}
		return nil
	}
	if r.HostIdentity != "" && r.HostIdentity != next.HostID {
		return errors.New("bound host identity changed; explicitly bind to the intended host")
	}
	next.Version = 7
	if old != nil && old.Phase != "destroyed" {
		next.Removed = maps.Clone(old.Removed)
	}
	selected := map[string]bool{}
	for _, name := range next.Selected {
		selected[name] = true
	}
	// The scope is captured by the planner before merging local and host config.
	if len(selected) == 0 {
		return errors.New("partial deployment requires a captured service selection")
	}
	for name, slot := range next.ServiceStates {
		id := serviceIncarnation(old, name)
		if !selected[name] {
			if id == "" {
				return fmt.Errorf("service %s has no stable identity", name)
			}
			slot.Incarnation = id
			continue
		}
		supplied := r.Bindings[name]
		if id != "" {
			if r.ReAdd {
				return fmt.Errorf("service %s already exists; re-add cannot replace it", name)
			}
			if supplied == "" {
				return fmt.Errorf("service %s already exists; explicitly bind this directory with soar service bind before deploying", name)
			}
			if supplied != id {
				return fmt.Errorf("service %s binding is stale; inspect and explicitly bind to its current incarnation", name)
			}
			slot.Incarnation = id
			continue
		}
		removed := next.Removed[name] != "" || (old != nil && old.Phase == "destroyed")
		if (removed || supplied != "") && !r.ReAdd {
			return fmt.Errorf("service %s was removed or its binding is stale; explicit --re-add --service %s --yes is required", name, name)
		}
		slot.Incarnation = r.Operation
		delete(next.Removed, name)
	}
	return nil
}

func (m *Manager) enrichIdentityResponse(_ context.Context, r Request, hostID string, resp *Response) {
	if resp.Status == nil {
		return
	}
	resp.Status.HostIdentity = hostID
	j, err := m.load(r.App)
	if err != nil {
		return
	}
	if j.Version == 1 && j.Phase != "destroyed" && resp.Status.Services == nil && (j.Current != nil || j.Candidate != nil) {
		s := ServiceStatus{Kind: "public", Phase: j.Phase, URL: j.URL, Container: resp.Status.Container, Running: resp.Status.Running, Available: resp.Status.Running, Operation: j.Operation}
		if j.Current != nil {
			s.Image = j.Current.Image
			s.Port = j.Current.Port
			s.Operation = j.Current.Operation
		}
		if j.Candidate != nil {
			s.Candidate = j.Candidate.Container
		}
		resp.Status.Services = map[string]ServiceStatus{"web": s}
	}
	for name, s := range resp.Status.Services {
		s.Incarnation = serviceIncarnation(j, name)
		resp.Status.Services[name] = s
	}
}
