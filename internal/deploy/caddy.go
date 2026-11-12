package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
)

const baseCaddy = "{\n\tadmin 127.0.0.1:2019\n}\n"

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func (m *Manager) adapt(c context.Context, text string) ([]byte, error) {
	p := m.path("/etc/caddy/.soar-adapt.Caddyfile")
	if e := atomic(p, []byte(text), 0600); e != nil {
		return nil, e
	}
	defer os.Remove(p)
	return m.cmd(c, "caddy", "adapt", "--config", p, "--adapter", "caddyfile")
}
func (m *Manager) live(c context.Context) ([]byte, error) {
	r, _ := http.NewRequestWithContext(c, "GET", "http://127.0.0.1:2019/config/", nil)
	resp, e := m.HTTP.Do(r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("Caddy admin API unavailable")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, e
}
func (m *Manager) promote(c context.Context, j *journal) error {
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
	p := m.path("/etc/caddy/Caddyfile")
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
		if j.Version == 2 {
			j.Phase = "updating"
		} else {
			j.Phase = "promoting"
		}
		if e = m.save(j); e != nil {
			return e
		}
	}
	temp := filepath.Join(filepath.Dir(p), ".soar-next.Caddyfile")
	defer os.Remove(temp)
	if e = atomic(temp, []byte(next), 0644); e != nil {
		return e
	}
	if _, e = m.cmd(c, "caddy", "validate", "--config", temp, "--adapter", "caddyfile"); e != nil {
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
		_, reloadErr := m.cmd(c, "caddy", "reload", "--config", p, "--adapter", "caddyfile")
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

// Reconcile the disk-before-reload crash boundary even when readiness now fails.
// Never replace an unrelated durable or runtime configuration.
func (m *Manager) reconcilePending(c context.Context, j *journal) error {
	if j.NewCaddy == "" {
		return nil
	}
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
	p := m.path("/etc/caddy/Caddyfile")
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
