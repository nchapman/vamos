package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/firewall"
)

type portBinding = struct{ HostIP, HostPort string }

// Docker expands publish ranges and may reorder bindings on the same container
// port. Compare multisets, not order; EXPOSE-only (nil) effective entries are OK.
func inspectServicePorts(v *inspection, r runtimeRef, attached bool) error {
	expected := map[string][]portBinding{}
	for _, value := range r.Config.Ports {
		p, err := config.ParsePort(value)
		if err != nil {
			return err
		}
		for i := 0; i < p.Count; i++ {
			key := fmt.Sprintf("%d/%s", p.Container+i, p.Protocol)
			expected[key] = append(expected[key], portBinding{"0.0.0.0", strconv.Itoa(p.Host + i)})
		}
	}
	if r.Config.HTTP != nil {
		if !validRuntimePort(*r.PublishPort) || (*r.Port != "" && *r.Port != *r.PublishPort) {
			return errors.New("invalid HTTP publish intent")
		}
		key := fmt.Sprintf("%d/tcp", r.Config.HTTP.Port)
		expected[key] = append(expected[key], portBinding{"127.0.0.1", *r.PublishPort})
	} else if *r.Port != "" || *r.PublishPort != "" {
		return errors.New("unexpected HTTP publish intent")
	}
	equal := func(want, actual []portBinding) bool {
		if len(want) != len(actual) {
			return false
		}
		counts := map[portBinding]int{}
		for _, b := range want {
			counts[b]++
		}
		for _, b := range actual {
			counts[b]--
			if counts[b] < 0 {
				return false
			}
		}
		return true
	}
	if len(expected) != len(v.HostConfig.PortBindings) {
		return errors.New("runtime published ports mismatch")
	}
	for key, want := range expected {
		if !equal(want, v.HostConfig.PortBindings[key]) {
			return errors.New("runtime published ports mismatch")
		}
	}
	for key, actual := range v.NetworkSettings.Ports {
		if len(actual) > 0 && !equal(expected[key], actual) {
			return errors.New("runtime effective ports mismatch")
		}
	}
	// Restart backoff and stopped/disconnected containers may have no effective
	// bindings. Their immutable HostConfig still has to match exactly.
	if attached && v.State.Running && !v.State.Restarting {
		for key, want := range expected {
			if !equal(want, v.NetworkSettings.Ports[key]) {
				return errors.New("runtime effective ports mismatch")
			}
		}
	}
	return nil
}

// Claims include desired configs and live/in-flight releases, not historical
// snapshots. Replacing one's own binding is allowed; transferring it to another
// service requires explicitly removing the old service/binding first.
func reserveJournalPorts(all []*journal) error {
	type claim struct {
		mapping config.PortMapping
		owner   hostnameOwner
		public  bool
	}
	var claims []claim
	add := func(app, name string, p config.PortMapping, public bool) error {
		owner := hostnameOwner{app, name}
		for _, prior := range claims {
			if p.Overlaps(prior.mapping) && (owner != prior.owner || public != prior.public) {
				return fmt.Errorf("host port %d/%s conflicts between %s/%s and %s/%s", max(p.Host, prior.mapping.Host), p.Protocol, prior.owner.app, prior.owner.service, app, name)
			}
		}
		claims = append(claims, claim{p, owner, public})
		return nil
	}
	cfg := func(app, name string, s *config.Service) error {
		if s == nil {
			return nil
		}
		for _, value := range s.Ports {
			p, err := config.ParsePort(value)
			if err != nil {
				return err
			}
			if err = add(app, name, p, true); err != nil {
				return err
			}
		}
		return nil
	}
	runtime := func(app, name string, r *release) error {
		if r == nil {
			return nil
		}
		if err := cfg(app, name, r.Config); err != nil {
			return err
		}
		for _, ref := range runtimeRefs(r) {
			port := *ref.PublishPort
			if port == "" {
				port = *ref.Port
			}
			if port == "" {
				continue
			}
			n, err := strconv.Atoi(port)
			if err != nil {
				return err
			}
			if err = add(app, name, config.PortMapping{Host: n, Count: 1, Protocol: "tcp"}, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, j := range all {
		if j.Phase == "destroyed" {
			continue
		}
		for name, s := range j.Config.Services {
			if err := cfg(j.App, name, &s); err != nil {
				return err
			}
		}
		if !isMulti(j) {
			for _, r := range []*release{j.Current, j.Candidate} {
				if err := runtime(j.App, "web", r); err != nil {
					return err
				}
			}
		}
		for name, slot := range j.ServiceStates {
			for _, r := range []*release{slot.Current, slot.Candidate, slot.Retiring} {
				if err := runtime(j.App, name, r); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Preflight includes persisted own claims before supersede removes candidates.
func (m *Manager) reservePorts(ctx context.Context, j *journal) error {
	unlock, err := m.lockIngress(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	all, _, err := m.routeJournals(j)
	if err != nil {
		return err
	}
	return m.reserveManagedPorts(all, j.HostID)
}

func (m *Manager) saveClaims(ctx context.Context, j *journal) error {
	return m.publishClaims(ctx, j, nil)
}

// Check host-wide claims and publish the journal under one ingress lock.
// Prepare durable inputs only after admission so a losing first deploy does
// not leave an archive in an unjournaled app directory.
func (m *Manager) publishClaims(ctx context.Context, j *journal, prepare func() error) error {
	unlock, err := m.lockIngress(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// The app mutation lock protects our own journal. At publication the new
	// journal contains all live own slots; superseded candidates have already
	// been removed. Preflight separately checks the persisted own claims.
	all, domain, err := m.appJournals(j, false)
	if err != nil {
		return err
	}
	if err = m.reserveManagedPorts(all, j.HostID); err != nil {
		return err
	}
	if err = reserveJournalHostnames(all, domain); err != nil {
		return err
	}
	if prepare != nil {
		if err = prepare(); err != nil {
			return err
		}
	}
	return m.save(j)
}

func (m *Manager) reserveManagedPorts(all []*journal, hostID string) error {
	sshPorts, err := firewall.SSHAllowances(m.Root, hostID)
	if err != nil {
		return err
	}
	for _, j := range all {
		if j.Phase == "destroyed" {
			continue
		}
		for name, cfg := range j.Config.Services {
			for _, value := range cfg.Ports {
				binding, err := config.ParsePort(value)
				if err != nil {
					return err
				}
				for _, port := range sshPorts {
					if binding.Overlaps(config.PortMapping{Host: port, Count: 1, Protocol: "tcp"}) {
						return fmt.Errorf("%s/%s: host port %d/tcp is reserved for management SSH", j.App, name, port)
					}
				}
			}
		}
	}
	return reserveJournalPorts(all)
}

// Leaving host mode or removing ports also uses stop-first, including recovery from an unavailable
// fixed-port release, even if its first deployment never became current.
// Storage identity itself is immutable.
func (s *serviceState) stopFirst(cfg config.Service) bool {
	if cfg.NetworkMode == "host" || cfg.StopFirst() || (s != nil && s.Unavailable) {
		return true
	}
	if s == nil || s.Current == nil || s.Current.Config == nil {
		return false
	}
	prior := s.Current.Config
	return prior.NetworkMode == "host" || len(prior.Ports) != 0
}
