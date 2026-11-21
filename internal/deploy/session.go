package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// SessionBinding optionally pins a project-local service to its remote identity.
type SessionBinding struct {
	HostIdentity string `json:"host_identity"`
	Incarnation  string `json:"incarnation"`
}

// SessionRequest contains only session metadata, never build inputs or secrets.
// Logs always follows; recorded run operations are not session targets.
type SessionRequest struct {
	Replica   int                       `json:"replica,omitempty"`
	Version   int                       `json:"version"`
	Action    string                    `json:"action"`
	Host      lifecycle.Config          `json:"host"`
	App       string                    `json:"app"`
	Service   string                    `json:"service,omitempty"`
	Bindings  map[string]SessionBinding `json:"bindings,omitempty"`
	Command   []string                  `json:"command,omitempty"`
	Stdin     bool                      `json:"stdin,omitempty"`
	TTY       bool                      `json:"tty,omitempty"`
	Candidate bool                      `json:"candidate,omitempty"`
	Tail      int                       `json:"tail,omitempty"`
}

// SessionTarget is a comparable admission snapshot. Attach must resolve again
// and compare the entire target, then address Docker by ContainerID, never by
// the display name. The admission lock is not held for the session lifetime.
type SessionTarget struct {
	Replica      int    `json:"replica,omitempty"`
	HostIdentity string `json:"host_identity"`
	App          string `json:"app"`
	Service      string `json:"service"`
	Incarnation  string `json:"incarnation"`
	Operation    string `json:"operation"`
	ContainerID  string `json:"container_id"`
	Container    string `json:"container"`
	Candidate    bool   `json:"candidate"`
}

var sessionContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateSessionRequest checks the bounded, action-specific session envelope.
// Command arguments are validated without trimming, parsing, or rewriting them.
func ValidateSessionRequest(r SessionRequest) error {
	if r.Replica < 0 || r.Replica > config.MaxAppReplicas {
		return fmt.Errorf("replica ordinal must be between 1 and %d", config.MaxAppReplicas)
	}
	if r.Version != 1 {
		return errors.New("unsupported session request version")
	}
	if r.Action != "exec" && r.Action != "logs" {
		return errors.New("session action must be exec or logs")
	}
	if len(r.App) > 63 || !label.MatchString(r.App) {
		return errors.New("invalid app name")
	}
	if r.Service != "" && (len(r.Service) > 63 || !label.MatchString(r.Service)) {
		return errors.New("invalid service selector")
	}
	if err := lifecycle.ValidateConfig(r.Host); err != nil {
		return err
	}
	if len(r.App+"."+r.Host.Domain) > 253 {
		return errors.New("generated hostname too long")
	}
	if len(r.Bindings) > 64 {
		return errors.New("at most 64 session bindings are supported")
	}
	for name, binding := range r.Bindings {
		if len(name) > 63 || !label.MatchString(name) || !operation.MatchString(binding.HostIdentity) || !operation.MatchString(binding.Incarnation) {
			return errors.New("invalid session binding identity")
		}
	}
	if r.Action == "logs" {
		if len(r.Command) != 0 || r.Stdin || r.TTY {
			return errors.New("logs cannot supply command, stdin, or tty")
		}
		if r.Tail < 1 || r.Tail > 1000 {
			return errors.New("logs tail must be between 1 and 1000")
		}
		return nil
	}
	if r.Candidate || r.Tail != 0 {
		return errors.New("exec cannot select candidate or supply tail")
	}
	if r.TTY && !r.Stdin {
		return errors.New("tty requires stdin")
	}
	if len(r.Command) == 0 || len(r.Command) > 256 || r.Command[0] == "" {
		return errors.New("exec requires a nonempty executable and at most 256 arguments")
	}
	total := 0
	for _, arg := range r.Command {
		if len(arg) > 8192 || !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') {
			return errors.New("exec arguments must be valid UTF-8 without NUL and at most 8 KiB each")
		}
		total += len(arg)
	}
	if total > 32<<10 {
		return errors.New("exec command exceeds 32 KiB")
	}
	return nil
}

// ResolveSession performs bounded, read-only target resolution. Exec refuses
// admission during host mutation and holds the host flock through inspection.
// Logs is observational and can inspect stopped containers during mutation.
// A later deployment may stop an admitted session; callers must not retarget it.
func (m *Manager) ResolveSession(ctx context.Context, r SessionRequest) (SessionTarget, error) {
	var zero SessionTarget
	if err := ValidateSessionRequest(r); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if r.Action == "exec" {
		if m.MutationLock != nil {
			active, unlock, lockErr := m.MutationLock(ctx, r.App, false)
			if lockErr != nil {
				return zero, lockErr
			}
			if unlock != nil {
				defer unlock()
			}
			if active {
				return zero, errors.New("host mutation already active")
			}
		} else {
			p := m.path(lifecycle.LockPath)
			if err := safe(p); err != nil {
				return zero, err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				return zero, err
			}
			lock := flock.New(p, flock.SetPermissions(0600))
			defer lock.Close()
			ok, err := lock.TryRLock()
			if err != nil {
				return zero, err
			}
			if !ok {
				return zero, errors.New("host mutation already active")
			}
			defer func() { _ = lock.Unlock() }()

			coordDir := m.path("/run/lock/vamos")
			if err := safe(coordDir); err != nil {
				return zero, err
			}
			hostOpPath := filepath.Join(coordDir, "host-op.lock")
			if _, statErr := os.Lstat(hostOpPath); statErr == nil {
				hostOpLock := flock.New(hostOpPath, flock.SetPermissions(0600))
				defer hostOpLock.Close()
				ok, err := hostOpLock.TryRLock()
				if err != nil {
					return zero, err
				}
				if !ok {
					return zero, errors.New("host mutation already active")
				}
				defer func() { _ = hostOpLock.Unlock() }()
			}
			appPath := filepath.Join(coordDir, "app-"+r.App+".lock")
			if _, statErr := os.Lstat(appPath); statErr == nil {
				appLock := flock.New(appPath, flock.SetPermissions(0600))
				defer appLock.Close()
				ok, err := appLock.TryLock()
				if err != nil {
					return zero, err
				}
				if !ok {
					return zero, errors.New("host mutation already active")
				}
				defer func() { _ = appLock.Unlock() }()
			}
		}
	}
	h, err := lifecycle.ReadState(m.path(lifecycle.StatePath))
	if err != nil {
		return zero, err
	}
	if h == nil || h.Phase != "ready" || h.Config != r.Host {
		return zero, errors.New("expected ready host identity does not match")
	}
	j, err := m.load(r.App)
	if err != nil {
		return zero, err
	}
	if j == nil || j.Phase == "destroyed" {
		return zero, errors.New("app has no recorded deployment")
	}
	if j.HostID != h.ID || j.URL != "https://"+r.App+"."+h.Config.Domain {
		return zero, errors.New("foreign app journal")
	}
	name := r.Service
	var slot *serviceState
	var rel *release
	if isMulti(j) {
		if name == "" {
			if j.ServiceStates["web"] != nil {
				name = "web"
			} else if len(j.ServiceStates) == 1 {
				for key := range j.ServiceStates {
					name = key
				}
			} else {
				return zero, errors.New("session target is ambiguous; select a service with --service")
			}
		}
		slot = j.ServiceStates[name]
		if slot == nil {
			return zero, errors.New("service has no recorded deployment")
		}
		rel = slot.Current
		if r.Candidate {
			rel = slot.Candidate
		}
	} else {
		if name != "" && name != "web" {
			return zero, errors.New("service has no recorded deployment")
		}
		name = "web"
		rel = j.Current
		if r.Candidate {
			rel = j.Candidate
			// Legacy draining retains the old release in Candidate, not a
			// pending replacement (matching the existing logs read path).
			if j.Phase == "draining" {
				rel = nil
			}
		}
	}
	if rel == nil {
		return zero, errors.New("selected session target has no recorded container")
	}
	ref, err := selectRuntime(rel, r.Replica)
	if err != nil {
		return zero, err
	}
	incarnation := serviceIncarnation(j, name)
	if binding, ok := r.Bindings[name]; ok && (binding.HostIdentity != h.ID || binding.Incarnation != incarnation) {
		return zero, errors.New("selected service binding does not match host or incarnation")
	}
	if r.Action == "exec" {
		if j.Maintenance != nil && (j.Maintenance.Service == "" || j.Maintenance.Service == name) {
			return zero, errors.New("selected service has active maintenance")
		}
		if j.Phase == "destroying" || (j.Phase == "removing" && j.Maintenance == nil) || (slot != nil && (slot.CurrentRetired || slot.Unavailable)) {
			return zero, errors.New("selected service is retired or unavailable")
		}
	}
	var v *inspection
	if slot != nil {
		v, err = m.instanceInspect(ctx, j, ref, r.Candidate && slot.Phase == "aliasing", !r.Candidate)
	} else {
		v, err = m.container(ctx, j, rel)
	}
	if err != nil {
		return zero, err
	}
	if !sessionContainerID.MatchString(v.ID) {
		return zero, errors.New("inspection has no immutable container ID")
	}
	if r.Action == "exec" && (!v.State.Running || v.State.Paused || v.State.Restarting || v.State.Dead || v.HostConfig.RestartPolicy.Name == "no") {
		return zero, errors.New("selected exec container is stopped, paused, retired, or unavailable")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return SessionTarget{HostIdentity: h.ID, App: j.App, Service: name, Incarnation: incarnation, Operation: rel.Operation, ContainerID: v.ID, Container: ref.Container, Replica: ref.Ordinal, Candidate: r.Candidate}, nil
}
