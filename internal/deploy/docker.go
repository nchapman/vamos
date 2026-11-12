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
	"strconv"
	"strings"
	"time"
)

type inspection struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount int
	Driver       string
	Scope        string
	EnableIPv6   bool
	EnableIPv4   *bool
	Ingress      bool
	Options      map[string]string
	Internal     bool
	Labels       map[string]string
	Containers   map[string]json.RawMessage
	Config       struct {
		Env        []string
		Cmd        []string
		Entrypoint []string
		User       string
		WorkingDir string
		Image      string
		Labels     map[string]string
		Volumes    map[string]json.RawMessage
	}
	HostConfig struct {
		NetworkMode                    string
		Privileged                     bool
		PublishAllPorts                bool
		ReadonlyRootfs                 bool
		CapAdd, CapDrop                []string
		Devices                        []json.RawMessage
		DeviceRequests                 []json.RawMessage
		VolumesFrom, Links, ExtraHosts []string
		DNS                            []string `json:"Dns"`
		DNSOptions                     []string `json:"DnsOptions"`
		DNSSearch                      []string `json:"DnsSearch"`
		SecurityOpt                    []string
		PidMode, IpcMode, UTSMode      string
		Mounts                         []json.RawMessage
		Binds                          []string
		RestartPolicy                  struct{ Name string }
		PortBindings                   map[string][]struct{ HostIP, HostPort string }
	}
	State struct {
		Running    bool
		Restarting bool
		Paused     bool
		Dead       bool
		StartedAt  string
	}
	Mounts          []json.RawMessage
	NetworkSettings struct {
		Networks map[string]json.RawMessage
		Ports    map[string][]struct{ HostIP, HostPort string }
	}
}

func (m *Manager) inspect(c context.Context, kind, name string) (*inspection, error) {
	b, e := m.cmd(c, "docker", kind, "inspect", name)
	if e != nil {
		return nil, e
	}
	var v []inspection
	if e = json.Unmarshal(b, &v); e != nil {
		return nil, errors.New("invalid Docker inspection response")
	}
	if len(v) != 1 {
		return nil, errors.New("expected one inspected object")
	}
	return &v[0], nil
}
func owned(labels map[string]string, j *journal, op string) bool {
	return labels["soar.host"] == j.HostID && labels["soar.app"] == j.App && (op == "" || labels["soar.operation"] == op)
}
func network(j *journal) string { return "soar-" + j.App }
func (m *Manager) container(c context.Context, j *journal, r *release) (*inspection, error) {
	v, e := m.inspect(c, "container", r.Container)
	if e != nil {
		return nil, e
	}
	if !owned(v.Config.Labels, j, r.Operation) || v.Config.Image != r.Image || len(v.Mounts) != 0 || len(v.NetworkSettings.Networks) != 1 || v.NetworkSettings.Networks[network(j)] == nil {
		return nil, errors.New("foreign or unsafe container")
	}
	if r.PublishPort != "" {
		key := ""
		for k := range v.HostConfig.PortBindings {
			key = k
		}
		configured := v.HostConfig.PortBindings[key]
		if len(v.HostConfig.PortBindings) != 1 || len(configured) != 1 || configured[0].HostIP != "127.0.0.1" || configured[0].HostPort != r.PublishPort {
			return nil, errors.New("configured port differs from recorded intent")
		}
		if v.State.Running {
			effective := v.NetworkSettings.Ports[key]
			if len(effective) != 1 || effective[0].HostIP != "127.0.0.1" || effective[0].HostPort != r.PublishPort {
				return nil, errors.New("effective port differs from recorded intent")
			}
		}
	}
	return v, nil
}

// List before inspect: an inspection failure is never interpreted as absence.
func (m *Manager) exists(c context.Context, kind, name string) (bool, error) {
	args := []string{kind, "ls", "--format", "{{json .}}"}
	if kind == "container" {
		args = append(args, "--all")
	}
	b, e := m.cmd(c, "docker", args...)
	if e != nil {
		return false, e
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		// Formatting metadata can contain objects (e.g. Docker 29 Platform).
		// Decode only identity fields rather than assuming every value is text.
		var row struct{ Name, Names string }
		if e = json.Unmarshal([]byte(line), &row); e != nil {
			return false, e
		}
		if row.Name == "" && row.Names == "" {
			return false, errors.New("Docker inventory row has no resource name")
		}
		if row.Name == name || row.Names == name {
			return true, nil
		}
	}
	return false, nil
}
func (m *Manager) ensureNetwork(c context.Context, j *journal) (err error) {
	defer func() {
		if err != nil && j.ServiceStates != nil {
			err = errors.New("ensure app network failed")
		}
	}()
	n := network(j)
	yes, e := m.exists(c, "network", n)
	if e != nil {
		return e
	}
	if !yes {
		if _, e = m.cmd(c, "docker", "network", "create", "--driver", "bridge", "--label", "soar.host="+j.HostID, "--label", "soar.app="+j.App, n); e != nil {
			return e
		}
	}
	v, e := m.inspect(c, "network", n)
	if e != nil {
		return e
	}
	return m.validateAppNetwork(c, j, v)
}

// Empty optional fields are accepted for older daemons and legacy test fixtures.
// Non-default bridge policy must not silently weaken app isolation.
func (m *Manager) validateAppNetwork(c context.Context, j *journal, v *inspection) error {
	if !owned(v.Labels, j, "") || v.Driver != "bridge" || v.Internal || v.EnableIPv6 || v.Ingress || (v.EnableIPv4 != nil && !*v.EnableIPv4) || (v.Scope != "" && v.Scope != "local") {
		return errors.New("foreign or unsafe app network")
	}
	defaults := map[string]string{
		"com.docker.network.bridge.enable_icc":           "true",
		"com.docker.network.bridge.enable_ip_masquerade": "true",
		"com.docker.network.bridge.gateway_mode_ipv4":    "nat",
		"com.docker.network.bridge.gateway_mode_ipv6":    "nat",
		"com.docker.network.bridge.host_binding_ipv4":    "0.0.0.0",
		"com.docker.network.bridge.inhibit_ipv4":         "false",
		"com.docker.network.bridge.enable_ip_forwarding": "true",
	}
	for key, value := range v.Options {
		if expected, ok := defaults[key]; !ok || value != expected {
			return errors.New("unsupported app network options")
		}
	}
	for id := range v.Containers {
		v, e := m.inspect(c, "container", id)
		if e != nil {
			return e
		}
		if !owned(v.Config.Labels, j, "") {
			return errors.New("foreign container on app network")
		}
		if j.ServiceStates != nil {
			if e := validateMultiEndpoint(j, v); e != nil {
				return e
			}
		}
	}
	return nil
}
func wait(c context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.Done():
		return c.Err()
	case <-t.C:
		return nil
	}
}
func (m *Manager) healthy(c context.Context, j *journal, r *release) error {
	c, cancel := context.WithTimeout(c, m.HealthTimeout)
	defer cancel()
	for {
		v, e := m.container(c, j, r)
		if e != nil {
			return e
		}
		if !v.State.Running {
			return errors.New("candidate is not running")
		}
		ports := v.NetworkSettings.Ports[fmt.Sprint(j.Config.Services["web"].HTTP.Port)+"/tcp"]
		if len(ports) != 1 || ports[0].HostIP != "127.0.0.1" {
			return errors.New("candidate must publish exactly one loopback port")
		}
		p, e := strconv.Atoi(ports[0].HostPort)
		if e != nil || p < 1 || p > 65535 {
			return errors.New("invalid published port")
		}
		if r.PublishPort != "" && r.PublishPort != ports[0].HostPort {
			return errors.New("published port differs from recorded intent")
		}
		if r.Port != "" && r.Port != ports[0].HostPort {
			return errors.New("published port changed")
		}
		r.Port = ports[0].HostPort
		req, _ := http.NewRequestWithContext(c, "GET", "http://127.0.0.1:"+r.Port+j.Config.Services["web"].HTTP.Health, nil)
		resp, e := m.HTTP.Do(req)
		if e == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if e = wait(c, 250*time.Millisecond); e != nil {
			return fmt.Errorf("HTTP readiness: %w", e)
		}
	}
}
func readContextArchive(path string) ([]byte, error) {
	if err := safe(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("saved context must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 32<<20 {
		return nil, errors.New("invalid saved context size")
	}
	return data, nil
}

func (m *Manager) advance(c context.Context, j *journal) error {
	if j.Phase == "ready" {
		return nil
	}
	s := j.Config.Services["web"]
	// Validate captured versions before any resumed build or runtime mutation.
	if _, err := m.secretValues(j, j.SecretRefs); err != nil {
		return err
	}
	if j.Candidate == nil {
		p := filepath.Join(m.dir(j.App), j.Operation+".image.iid")
		if e := safe(p); e != nil {
			return e
		}
		if e := os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		archive := j.Archive
		if archive == "" {
			archive = "context.tar"
		}
		tar, e := readContextArchive(filepath.Join(m.dir(j.App), archive))
		if e != nil {
			return e
		}
		if len(tar) == 0 || len(tar) > 32<<20 {
			return errors.New("invalid saved context")
		}
		// Bootstrap disables Docker's shared default bridge. Trusted builds use
		// host networking; runtime containers still get isolated app bridges.
		out, e := m.Run(c, "docker", []string{"buildx", "build", "--load", "--network", "host", "--iidfile", p, "--file", s.Build.Dockerfile, "-"}, tar)
		if e != nil {
			return fmt.Errorf("build: %w: %s", e, diagnostic(string(out.Stderr)))
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		id := strings.TrimSpace(string(b))
		if !imageID.MatchString(id) {
			return errors.New("build did not produce immutable image ID")
		}
		v, e := m.inspect(c, "image", id)
		if e != nil {
			return e
		}
		if v.ID != id || len(v.Config.Volumes) > 0 {
			return errors.New("image declares volumes or mismatched identity")
		}
		j.Candidate = &release{Container: "soar-" + j.App + "-" + j.Operation, Image: id, Operation: j.Operation, SecretRefs: j.SecretRefs}
		j.Phase = "starting"
		if e = m.save(j); e != nil {
			return e
		}
	}
	r := j.Candidate
	if r == nil || (j.Phase != "draining" && collision(r, j.Current)) || collision(r, j.Previous) {
		return errors.New("candidate aliases retained release")
	}
	if j.Phase != "draining" {
		if e := m.reconcilePending(c, j); e != nil {
			return e
		}
		if e := m.ensureNetwork(c, j); e != nil {
			return e
		}
		yes, e := m.exists(c, "container", r.Container)
		if e != nil {
			return e
		}
		if !yes {
			if r.Port != "" && r.PublishPort == "" {
				return errors.New("recorded endpoint missing; refusing recreation")
			}
			if r.PublishPort == "" {
				allocator := m.AllocatePort
				if allocator == nil {
					allocator = allocatePort
				}
				port, err := allocator()
				if err != nil {
					return err
				}
				p, err := strconv.Atoi(port)
				if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
					return errors.New("invalid allocated port")
				}
				r.PublishPort = port
				// Persist intent before Docker can create anything, including a lost response.
				if err := m.save(j); err != nil {
					return err
				}
			}
			v, e := m.inspect(c, "image", r.Image)
			if e != nil {
				return e
			}
			if len(v.Config.Volumes) > 0 {
				return errors.New("image declares volumes")
			}
			args := []string{"run", "--detach", "--name", r.Container, "--restart", "unless-stopped", "--network", network(j), "--publish", fmt.Sprintf("127.0.0.1:%s:%d", r.PublishPort, s.HTTP.Port), "--label", "soar.host=" + j.HostID, "--label", "soar.app=" + j.App, "--label", "soar.operation=" + j.Operation}
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				args = append(args, "--env", k+"="+s.Env[k])
			}
			args = append(args, r.Image)
			args = append(args, s.Command...)
			if e = m.runContainer(c, j, r, args); e != nil {
				return e
			}
		}
		v, e := m.candidate(c, j, r)
		if e != nil {
			return e
		}
		if !v.State.Running {
			if _, e = m.cmd(c, "docker", "start", r.Container); e != nil {
				return e
			}
		}
		if e = m.healthy(c, j, r); e != nil {
			return e
		}
		if e = m.promote(c, j); e != nil {
			return e
		}
		j.Previous = j.Current
		j.Current = r
		j.Container = r.Container
		j.Phase = "draining"
		if e = m.save(j); e != nil {
			return e
		}
	}
	if j.Previous != nil {
		if e := wait(c, m.Drain); e != nil {
			return e
		}
		if _, e := m.container(c, j, j.Previous); e != nil {
			return e
		}
		if _, e := m.cmd(c, "docker", "update", "--restart=no", j.Previous.Container); e != nil {
			return e
		}
		if _, e := m.cmd(c, "docker", "stop", "--time", "30", j.Previous.Container); e != nil {
			return e
		}
	}
	j.Phase = "ready"
	j.LastError = ""
	j.Candidate = nil
	j.OldCaddy = ""
	j.NewCaddy = ""
	return m.save(j)
}

// Adoption requires a candidate that has not begun retirement.
func (m *Manager) candidate(c context.Context, j *journal, r *release) (*inspection, error) {
	if r.PublishPort == "" {
		return nil, errors.New("legacy dynamic candidate requires ordinary redeployment")
	}
	v, e := m.verifyCandidate(c, j, r)
	if e != nil {
		return nil, e
	}
	if v.HostConfig.RestartPolicy.Name != "unless-stopped" {
		return nil, errors.New("candidate restart policy mismatch")
	}
	return v, nil
}

// Cleanup may resume after disabling restart, but must verify all other settings.
func (m *Manager) cleanupCandidate(c context.Context, j *journal, r *release) (*inspection, error) {
	v, e := m.verifyCandidate(c, j, r)
	if e != nil {
		return nil, e
	}
	switch v.HostConfig.RestartPolicy.Name {
	case "unless-stopped", "no":
		return v, nil
	default:
		return nil, errors.New("candidate restart policy mismatch")
	}
}

// Verify identity and immutable settings against the captured request and image defaults.
// Callers separately enforce the restart policy appropriate to adoption or cleanup.
func (m *Manager) verifyCandidate(c context.Context, j *journal, r *release) (*inspection, error) {
	v, e := m.container(c, j, r)
	if e != nil {
		return nil, e
	}
	im, e := m.inspect(c, "image", r.Image)
	if e != nil {
		return nil, e
	}
	s := j.Config.Services["web"]
	env := map[string]string{}
	for _, x := range im.Config.Env {
		k, val, ok := strings.Cut(x, "=")
		if ok {
			env[k] = val
		}
	}
	for k, val := range s.Env {
		env[k] = val
	}
	values, e := m.secretValues(j, r.SecretRefs)
	if e != nil {
		return nil, e
	}
	for key, value := range values {
		env[key] = value
	}
	actual := map[string]string{}
	for _, x := range v.Config.Env {
		k, val, ok := strings.Cut(x, "=")
		if ok {
			actual[k] = val
		}
	}
	cmd := s.Command
	if len(cmd) == 0 {
		cmd = im.Config.Cmd
	}
	ports := v.HostConfig.PortBindings[fmt.Sprint(s.HTTP.Port)+"/tcp"]
	if !reflect.DeepEqual(env, actual) || !reflect.DeepEqual(cmd, v.Config.Cmd) || v.HostConfig.Privileged || len(v.HostConfig.Binds) != 0 || v.HostConfig.NetworkMode != network(j) || len(v.HostConfig.PortBindings) != 1 || len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || (r.PublishPort != "" && ports[0].HostPort != r.PublishPort) {
		return nil, errors.New("candidate immutable configuration mismatch")
	}
	return v, nil
}
