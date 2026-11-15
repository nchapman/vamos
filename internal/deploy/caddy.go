package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"

	"github.com/nchapman/vamos/internal/lifecycle"
)

const baseCaddy = lifecycle.BaseCaddyfile

// caddy runs the Caddy CLI inside the managed ingress container. /etc/caddy is
// mounted at the same path, so arguments use host paths without the test root.
// Caddy's own diagnostics carry no secrets, unlike app container output.
func (m *Manager) caddy(c context.Context, args ...string) ([]byte, error) {
	name, argv := lifecycle.CaddyCommand(args...)
	r, e := m.Run(c, name, argv, nil)
	if e != nil {
		if c.Err() != nil {
			return nil, fmt.Errorf("caddy %s: %w", args[0], c.Err())
		}
		return nil, fmt.Errorf("caddy %s: %w: %s", args[0], e, diagnostic(string(r.Stderr)))
	}
	return r.Stdout, nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func (m *Manager) adapt(c context.Context, text string) ([]byte, error) {
	const name = "/etc/caddy/.vamos-adapt.Caddyfile"
	p := m.path(name)
	if e := atomic(p, []byte(text), 0600); e != nil {
		return nil, e
	}
	defer os.Remove(p)
	return m.caddy(c, "adapt", "--config", name, "--adapter", "caddyfile")
}
func (m *Manager) live(c context.Context) ([]byte, error) {
	r, _ := http.NewRequestWithContext(c, "GET", lifecycle.CaddyAdminURL, nil)
	resp, e := m.HTTP.Do(r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("Caddy admin API unavailable")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if e == nil && len(b) > 4<<20 {
		return nil, errors.New("Caddy admin configuration exceeds 4 MiB")
	}
	return b, e
}
func (m *Manager) promote(c context.Context, j *journal) error {
	unlock, err := m.lockIngress(c)
	if err != nil {
		return err
	}
	defer unlock()
	if e := validateRouteCandidate(j); e != nil {
		return e
	}
	old, e := m.routes(j, false)
	if e != nil {
		return e
	}
	next, e := m.routes(j, true)
	if e != nil {
		return e
	}
	if j.NewCaddy != "" && (j.OldCaddy != old || j.NewCaddy != next) {
		return errors.New("promotion intent differs from journals")
	}
	p := m.path(lifecycle.CaddyfilePath)
	if e = safe(p); e != nil {
		return e
	}
	file, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	pending := j.NewCaddy != ""
	if string(file) != old && (!pending || string(file) != next) {
		return errors.New("Caddyfile manual drift")
	}
	oldJSON, e := m.adapt(c, old)
	if e != nil {
		return e
	}
	newJSON, e := m.adapt(c, next)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	isNew := sameJSON(live, newJSON)
	if !sameJSON(live, oldJSON) && (!pending || !isNew) {
		return errors.New("Caddy runtime drift")
	}
	if !pending {
		j.OldCaddy = old
		j.NewCaddy = next
		if isMulti(j) {
			j.Phase = "updating"
		} else {
			j.Phase = "promoting"
		}
		if e = m.save(j); e != nil {
			return e
		}
	}
	temp := filepath.Join(filepath.Dir(p), ".vamos-next.Caddyfile")
	defer os.Remove(temp)
	if e = atomic(temp, []byte(next), 0644); e != nil {
		return e
	}
	if _, e = m.caddy(c, "validate", "--config", "/etc/caddy/.vamos-next.Caddyfile", "--adapter", "caddyfile"); e != nil {
		return e
	}
	if e = m.routeCandidateHealthy(c, j); e != nil {
		return e
	}
	if string(file) != next {
		if e = atomic(p, []byte(next), 0644); e != nil {
			return e
		}
	}
	if !isNew {
		_, reloadErr := m.caddy(c, "reload", "--config", lifecycle.CaddyfilePath, "--adapter", "caddyfile")
		live, e = m.live(c)
		if e != nil {
			return errors.Join(reloadErr, e)
		}
		if !sameJSON(live, newJSON) {
			if sameJSON(live, oldJSON) {
				b, x := os.ReadFile(p)
				if x != nil {
					return x
				}
				if string(b) != next && string(b) != old {
					return errors.New("Caddyfile drift during reload")
				}
				if x = atomic(p, []byte(old), 0644); x != nil {
					return x
				}
			}
			return errors.Join(reloadErr, errors.New("Caddy switch not proven; promotion remains pending"))
		}
	}
	return nil
}

// verifyUnservedPromotion deliberately accepts only old-disk/old-live state.
// A disk-before-reload interruption must first be reconciled by resume; live-new,
// equivalent old/new routes, drift, and unavailable inspection are not proof that
// the candidate was never routed publicly.
func (m *Manager) verifyUnservedPromotion(c context.Context, j *journal) error {
	unlock, err := m.lockIngress(c)
	if err != nil {
		return err
	}
	defer unlock()
	if j.NewCaddy == "" {
		return errors.New("no pending promotion to cancel")
	}
	if err := validateRouteCandidate(j); err != nil {
		return err
	}
	old, err := m.routes(j, false)
	if err != nil {
		return err
	}
	next, err := m.routes(j, true)
	if err != nil {
		return err
	}
	if old != j.OldCaddy || next != j.NewCaddy {
		return errors.New("promotion intent differs from journals")
	}
	p := m.path(lifecycle.CaddyfilePath)
	if err = safe(p); err != nil {
		return err
	}
	file, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if string(file) != old {
		return errors.New("promotion cancellation requires the old Caddyfile; resume to reconcile")
	}
	oldJSON, err := m.adapt(c, old)
	if err != nil {
		return err
	}
	newJSON, err := m.adapt(c, next)
	if err != nil {
		return err
	}
	live, err := m.live(c)
	if err != nil {
		return err
	}
	if !sameJSON(live, oldJSON) || sameJSON(live, newJSON) {
		return errors.New("promotion is not proven unserved; resume before superseding")
	}
	return nil
}

// Keep the promotion fence until candidate removal is proven. If a response or
// this save is lost, explicit supersede can repeat proof and removal. Once saved,
// an interrupted supersede can also resume the original captured deployment,
// recreating its candidate without an alias on the same reserved port.
func (m *Manager) recordCancelledPromotion(j *journal) error {
	if j.NewCaddy == "" {
		return nil
	}
	candidate := j.Candidate
	if isMulti(j) {
		slot := j.ServiceStates[j.ActiveService]
		slot.Phase = "starting"
		candidate = slot.Candidate
	} else {
		j.Phase = "starting"
	}
	for _, ref := range runtimeRefs(candidate) {
		if *ref.PublishPort == "" {
			*ref.PublishPort = *ref.Port
		}
		*ref.Port = ""
	}
	j.OldCaddy, j.NewCaddy = "", ""
	return m.save(j)
}

// Reconcile the disk-before-reload crash boundary even when readiness now fails.
// Never replace an unrelated durable or runtime configuration.
func (m *Manager) reconcilePending(c context.Context, j *journal) error {
	if j.NewCaddy == "" {
		return nil
	}
	unlock, err := m.lockIngress(c)
	if err != nil {
		return err
	}
	defer unlock()
	if e := validateRouteCandidate(j); e != nil {
		return e
	}
	old, e := m.routes(j, false)
	if e != nil {
		return e
	}
	next, e := m.routes(j, true)
	if e != nil {
		return e
	}
	if old != j.OldCaddy || next != j.NewCaddy {
		return errors.New("promotion intent differs from journals")
	}
	p := m.path(lifecycle.CaddyfilePath)
	if e = safe(p); e != nil {
		return e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if string(b) != old && string(b) != next {
		return errors.New("Caddyfile drift")
	}
	a, e := m.adapt(c, old)
	if e != nil {
		return e
	}
	n, e := m.adapt(c, next)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	if sameJSON(live, a) {
		if string(b) != old {
			return atomic(p, []byte(old), 0644)
		}
		return nil
	}
	if !sameJSON(live, n) {
		return errors.New("Caddy runtime drift")
	}
	return nil
}

func (m *Manager) verifyCurrentRoutes(c context.Context, j *journal) error {
	unlock, err := m.lockIngress(c)
	if err != nil {
		return err
	}
	defer unlock()
	old, e := m.routes(j, false)
	if e != nil {
		return e
	}
	p := m.path(lifecycle.CaddyfilePath)
	if e = safe(p); e != nil {
		return e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if string(b) != old {
		return errors.New("Caddyfile drift")
	}
	expected, e := m.adapt(c, old)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	if !sameJSON(live, expected) {
		return errors.New("Caddy runtime drift")
	}
	return nil
}
