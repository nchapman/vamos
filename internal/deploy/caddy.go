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
	"sort"
	"strings"
)

const baseCaddy = "{\n\tadmin 127.0.0.1:2019\n}\n"

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func (m *Manager) routes(j *journal, candidate bool) (string, error) {
	entries, e := os.ReadDir(m.path("/var/lib/soar/apps"))
	if e != nil {
		return "", e
	}
	routes := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			return "", errors.New("unexpected app journal entry")
		}
		other, e := m.load(entry.Name())
		if e != nil {
			return "", e
		}
		if other.HostID != j.HostID || strings.TrimPrefix(other.URL, "https://"+other.App+".") != strings.TrimPrefix(j.URL, "https://"+j.App+".") {
			return "", errors.New("foreign app journal")
		}
		if other.App != j.App && other.NewCaddy != "" {
			return "", errors.New("another app has pending promotion; resume it first")
		}
		r := other.Current
		if other.App == j.App && candidate {
			r = j.Candidate
		}
		if r != nil {
			routes[strings.TrimPrefix(other.URL, "https://")] = r.Port
		}
	}
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := baseCaddy
	for _, k := range keys {
		out += fmt.Sprintf("\n%s {\n\treverse_proxy 127.0.0.1:%s\n}\n", k, routes[k])
	}
	return out, nil
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
		j.Phase = "promoting"
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
	if e = m.healthy(c, j, j.Candidate); e != nil {
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
