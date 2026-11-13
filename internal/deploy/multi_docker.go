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

	"github.com/nchapman/soar/internal/buildcontext"
)

// Runtime errors intentionally never contain Docker output, environment values,
// image metadata, filesystem paths, or HTTP response bodies.
func (m *Manager) multiBuild(ctx context.Context, j *journal, archives map[string][]byte) error {
	if j.Config == nil || !operation.MatchString(j.Operation) {
		return errors.New("invalid captured build configuration")
	}
	sources := activeSources(j)
	if len(archives) != len(buildcontext.Builds(bundleConfig(j))) {
		return errors.New("build archive set mismatch")
	}
	keys := make([]string, 0, len(sources))
	for key, cfg := range sources {
		if !operation.MatchString(key) || (cfg.Build != nil && len(archives[key]) == 0) {
			return errors.New("missing build archive")
		}
		keys = append(keys, key)
	}
	for key := range j.BuildImages {
		if _, ok := sources[key]; !ok {
			return errors.New("unexpected captured build image")
		}
	}
	sort.Strings(keys)
	if j.BuildImages == nil {
		j.BuildImages = map[string]string{}
	}
	for _, key := range keys {
		cfg := sources[key]
		id := j.BuildImages[key]
		if id == "" && cfg.Image != "" {
			var err error
			id, err = m.resolvePrebuilt(ctx, j, cfg)
			if err != nil {
				return err
			}
		}
		if id == "" {
			p := filepath.Join(m.dir(j.App), j.Operation+"."+key+".image.iid")
			if safe(p) != nil {
				return errors.New("unsafe build image file")
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("remove build image file failed")
			}
			// Only build archives, never runtime environment or secrets, enter builds.
			out, err := m.Run(ctx, "docker", []string{"buildx", "build", "--load", "--network", "host", "--iidfile", p, "--file", cfg.Build.Dockerfile, "-"}, archives[key])
			if err != nil {
				return errors.New(m.buildDiagnostic(j, out, err, diagnosticLimit))
			}
			if safe(p) != nil {
				return errors.New("unsafe build image file")
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return errors.New("read build image ID failed")
			}
			id = strings.TrimSpace(string(b))
		}
		if !imageID.MatchString(id) {
			return errors.New("build did not produce immutable image ID")
		}
		im, err := m.inspect(ctx, "image", id)
		if err != nil {
			return errors.New("inspect build image failed")
		}
		if im.ID != id {
			return errors.New("image identity mismatch")
		}
		for name, service := range j.Config.Services {
			if slot := j.ServiceStates[name]; slot != nil && slot.Skipped {
				continue
			}
			if imageKey(service) == key {
				if err := imageVolumesAllowed(im, &service, true); err != nil {
					return err
				}
			}
		}
		if j.BuildImages[key] == "" {
			j.BuildImages[key] = id
			if m.save(j) != nil {
				delete(j.BuildImages, key)
				return errors.New("save build image failed")
			}
		}
	}
	return nil
}

func multiIdentity(j *journal, r *release) error {
	if r == nil || r.Config == nil || !operation.MatchString(r.Operation) || !imageID.MatchString(r.Image) || (r.Service != "" && (!label.MatchString(r.Service) || len(r.Service) > 63)) || r.Container != containerName(j.App, r.Service, r.Operation) {
		return errors.New("invalid captured runtime identity")
	}
	if !matchingSecretRefs(r.SecretRefs, r.Config.Secrets) {
		return errors.New("captured runtime secrets mismatch")
	}
	if r.Config.Volume != nil {
		if !supportsVolumes(j) || r.Service == "" || !operation.MatchString(r.VolumeID) {
			return errors.New("invalid runtime volume identity")
		}
	} else if r.VolumeID != "" || r.ExecutionIntent {
		return errors.New("unexpected runtime volume intent")
	}
	if r.Service == "" && r.Config.HTTP == nil {
		return errors.New("invalid legacy runtime configuration")
	}
	return nil
}

func multiAliasing(j *journal, r *release) bool {
	if j.Phase != "updating" || r == nil || r.Operation != j.Operation || r.Service == "" || r.Config == nil || r.Config.HTTP == nil {
		return false
	}
	s := j.ServiceStates[r.Service]
	return s != nil && s.Phase == "aliasing" && s.Candidate != nil && s.Candidate.Container == r.Container && s.Candidate.Operation == r.Operation
}

type multiEndpoint struct{ Aliases []string }

func endpointAliases(v *inspection, n string) ([]string, error) {
	raw, ok := v.NetworkSettings.Networks[n]
	if !ok {
		return nil, errors.New("container endpoint missing")
	}
	var ep multiEndpoint
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &ep) != nil {
		return nil, errors.New("invalid container endpoint")
	}
	return ep.Aliases, nil
}
func hasAlias(v *inspection, n, alias string) bool {
	aliases, err := endpointAliases(v, n)
	if err != nil {
		return false
	}
	for _, a := range aliases {
		if a == alias {
			return true
		}
	}
	return false
}

// A name prefix is never authority. Network endpoints must have exact labels,
// exact names, and a retained identity. Only live slots may hold service aliases;
// historical snapshots are retention references, not DNS admission authority.
func validateMultiEndpoint(j *journal, v *inspection) error {
	service := v.Config.Labels["soar.service"]
	op := v.Config.Labels["soar.operation"]
	name := strings.TrimPrefix(v.Name, "/")
	if !operation.MatchString(op) || (service == "" && hasServiceLabel(v.Config.Labels)) || (service != "" && !label.MatchString(service)) || name != containerName(j.App, service, op) {
		return errors.New("foreign container on app network")
	}
	var ref *release
	for _, r := range retainedReleases(j) {
		if r.Container == name && r.Operation == op && r.Service == service {
			ref = r
			break
		}
	}
	if ref == nil {
		return errors.New("unrecorded container on app network")
	}
	if v.Config.Image != ref.Image {
		return errors.New("app network endpoint image mismatch")
	}
	aliases, err := endpointAliases(v, network(j))
	if err != nil {
		return err
	}
	allowed := false
	slot := j.ServiceStates[service]
	if slot != nil {
		for _, r := range []*release{slot.Current, slot.Retiring} {
			if r != nil && r.Container == name {
				allowed = true
			}
		}
		if r := slot.Candidate; r != nil && r.Container == name && r.Config != nil {
			allowed = r.Config.HTTP == nil || slot.Phase == "aliasing" || slot.Phase == "promoting"
		}
	}
	for _, a := range aliases {
		// Even Docker-generated name/ID aliases must not impersonate another
		// configured service's DNS label.
		_, configured := j.ServiceStates[a]
		if j.Config != nil {
			if _, ok := j.Config.Services[a]; ok {
				configured = true
			}
		}
		if configured {
			if a == service && allowed {
				continue
			}
			return errors.New("service alias belongs to another endpoint")
		}
		if a == name || (v.ID != "" && (a == v.ID || (len(v.ID) >= 12 && a == v.ID[:12]))) {
			continue
		}
		if a == service && allowed {
			continue
		}
		return errors.New("unexpected app network alias")
	}
	return nil
}

func validRuntimePort(port string) bool {
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p <= 65535 && strconv.Itoa(p) == port
}

// Inspection deliberately accepts restart=no for retirement recovery. Adoption,
// observation and readiness separately require unless-stopped. Detachment is
// authorized only by the recorded public-candidate aliasing phase.
func (m *Manager) multiInspect(ctx context.Context, j *journal, r *release, allowDetached, requireAlias bool) (*inspection, error) {
	if err := multiIdentity(j, r); err != nil {
		return nil, err
	}
	if allowDetached && !multiAliasing(j, r) {
		return nil, errors.New("detached inspection requires aliasing intent")
	}
	v, err := m.inspect(ctx, "container", r.Container)
	if err != nil {
		return nil, errors.New("inspect runtime container failed")
	}
	if !owned(v.Config.Labels, j, r.Operation) || v.Config.Labels["soar.service"] != r.Service || (r.Service == "" && hasServiceLabel(v.Config.Labels)) || ((r.Service != "" || v.Name != "") && strings.TrimPrefix(v.Name, "/") != r.Container) || v.Config.Image != r.Image || imageVolumesAllowed(v, r.Config, true) != nil {
		return nil, errors.New("foreign or unsafe runtime container")
	}
	if err := verifyResources(v, r.Config.Resources); err != nil {
		return nil, err
	}
	if err := m.verifyRuntimeMounts(ctx, j, r, v); err != nil {
		return nil, err
	}
	attached := len(v.NetworkSettings.Networks) == 1 && v.NetworkSettings.Networks[network(j)] != nil
	if !attached && !(allowDetached && len(v.NetworkSettings.Networks) == 0) {
		return nil, errors.New("runtime network mismatch")
	}
	if attached {
		if _, err := endpointAliases(v, network(j)); err != nil {
			return nil, err
		}
		if r.Service != "" {
			if requireAlias && !hasAlias(v, network(j), r.Service) {
				return nil, errors.New("service alias missing")
			}
			// Validate this endpoint even when a network inspect fixture omits its ID.
			copy := *v
			if copy.Name == "" {
				copy.Name = r.Container
			}
			if err := validateMultiEndpoint(j, &copy); err != nil {
				return nil, err
			}
		}
	}
	nw, err := m.inspect(ctx, "network", network(j))
	if err != nil {
		return nil, errors.New("inspect app network failed")
	}
	if err = m.validateAppNetwork(ctx, j, nw); err != nil {
		return nil, errors.New("app network ownership or isolation mismatch")
	}
	im, err := m.inspect(ctx, "image", r.Image)
	if err != nil {
		return nil, errors.New("inspect runtime image failed")
	}
	if im.ID != r.Image || imageVolumesAllowed(im, r.Config, true) != nil {
		return nil, errors.New("unsafe runtime image")
	}
	labels := map[string]string{}
	for key, value := range im.Config.Labels {
		labels[key] = value
	}
	labels["soar.host"] = j.HostID
	labels["soar.app"] = j.App
	labels["soar.operation"] = r.Operation
	if r.Service != "" {
		labels["soar.service"] = r.Service
	}
	if !reflect.DeepEqual(labels, v.Config.Labels) {
		return nil, errors.New("runtime label configuration mismatch")
	}
	env := map[string]string{}
	for _, x := range im.Config.Env {
		if k, val, ok := strings.Cut(x, "="); ok {
			env[k] = val
		}
	}
	for k, val := range r.Config.Env {
		env[k] = val
	}
	values, err := m.secretValues(j, r.SecretRefs)
	if err != nil {
		return nil, errors.New("read captured runtime secrets failed")
	}
	for k, val := range values {
		env[k] = val
	}
	actual := map[string]string{}
	for _, x := range v.Config.Env {
		k, val, ok := strings.Cut(x, "=")
		if !ok {
			return nil, errors.New("runtime environment mismatch")
		}
		if _, duplicate := actual[k]; duplicate {
			return nil, errors.New("runtime environment mismatch")
		}
		actual[k] = val
	}
	cmd := r.Config.Command
	if len(cmd) == 0 {
		cmd = im.Config.Cmd
	}
	if !reflect.DeepEqual(env, actual) || !reflect.DeepEqual(cmd, v.Config.Cmd) || !reflect.DeepEqual(im.Config.Entrypoint, v.Config.Entrypoint) || im.Config.User != v.Config.User || im.Config.WorkingDir != v.Config.WorkingDir || unsafeMultiHostConfig(v) || v.HostConfig.NetworkMode != network(j) {
		return nil, errors.New("runtime immutable configuration mismatch")
	}
	if policy := v.HostConfig.RestartPolicy.Name; policy != "unless-stopped" && policy != "no" {
		return nil, errors.New("runtime restart policy mismatch")
	}
	if r.Config.HTTP == nil {
		if r.Port != "" || r.PublishPort != "" || len(v.HostConfig.PortBindings) != 0 {
			return nil, errors.New("private service publishes ports")
		}
		for _, ports := range v.NetworkSettings.Ports {
			if len(ports) != 0 {
				return nil, errors.New("private service publishes ports")
			}
		}
	} else {
		key := fmt.Sprint(r.Config.HTTP.Port) + "/tcp"
		if r.Config.HTTP.Port < 1 || r.Config.HTTP.Port > 65535 {
			return nil, errors.New("invalid captured HTTP port")
		}
		ports := v.HostConfig.PortBindings[key]
		if len(v.HostConfig.PortBindings) != 1 || len(ports) != 1 || ports[0].HostIP != "127.0.0.1" {
			return nil, errors.New("runtime published ports mismatch")
		}
		if r.Service != "" && !validRuntimePort(r.PublishPort) {
			return nil, errors.New("missing explicit publish intent")
		}
		if r.PublishPort != "" {
			if !validRuntimePort(r.PublishPort) || ports[0].HostPort != r.PublishPort || (r.Port != "" && r.Port != r.PublishPort) {
				return nil, errors.New("runtime published port changed")
			}
		} else if r.Service != "" || !validRuntimePort(r.Port) || (ports[0].HostPort != "" && ports[0].HostPort != "0" && ports[0].HostPort != r.Port) {
			return nil, errors.New("legacy published port not proven")
		}
		for k, ports := range v.NetworkSettings.Ports {
			if k != key && len(ports) != 0 {
				return nil, errors.New("unexpected runtime published port")
			}
		}
		// Docker withdraws effective ports during restart backoff while reporting
		// Running=true. Static bindings remain mandatory; start/health admission
		// separately rejects Restarting. Inspection must still permit retirement.
		if attached && v.State.Running && !v.State.Restarting {
			effective := v.NetworkSettings.Ports[key]
			expected := r.PublishPort
			if expected == "" {
				expected = r.Port
			}
			if len(effective) != 1 || effective[0].HostIP != "127.0.0.1" || effective[0].HostPort != expected {
				return nil, errors.New("runtime effective port changed")
			}
		}
	}
	return v, nil
}

// Docker sets IpcMode=private by default; empty is accepted for historical
// inspections. No custom host integrations are part of the captured runtime.
func unsafeMultiHostConfig(v *inspection) bool {
	h := v.HostConfig
	return h.Privileged || h.PublishAllPorts || h.ReadonlyRootfs || len(h.Binds) != 0 || len(h.CapAdd) != 0 || len(h.CapDrop) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.VolumesFrom) != 0 || len(h.Links) != 0 || len(h.ExtraHosts) != 0 || len(h.DNS) != 0 || len(h.DNSOptions) != 0 || len(h.DNSSearch) != 0 || len(h.SecurityOpt) != 0 || h.PidMode != "" || h.UTSMode != "" || (h.IpcMode != "" && h.IpcMode != "private")
}

func hasServiceLabel(labels map[string]string) bool { _, ok := labels["soar.service"]; return ok }

func (m *Manager) multiEnsureCandidate(ctx context.Context, j *journal, r *release) error {
	return m.multiEnsureRuntime(ctx, j, r, false)
}

// Volume candidates are created stopped and inspected before route withdrawal
// or retiring a writer. The same captured identity is later started explicitly.
func (m *Manager) multiEnsureRuntime(ctx context.Context, j *journal, r *release, prepareOnly bool) error {
	if err := multiIdentity(j, r); err != nil {
		return err
	}
	if err := m.requireResourceSupport(ctx, r.Config.Resources); err != nil {
		return err
	}
	slot := j.ServiceStates[r.Service]
	if j.Phase != "updating" || r.Operation != j.Operation || r.Service == "" || slot == nil || slot.Candidate == nil || slot.Candidate.Container != r.Container || (!prepareOnly && slot.Phase != "starting") || (prepareOnly && (r.Config.Volume == nil || slot.Phase != "prepared")) {
		return errors.New("candidate creation requires starting intent")
	}
	if !prepareOnly && (r.Config.HTTP == nil || r.Config.Volume != nil) && slot.Current != nil {
		if !slot.CurrentRetired {
			return errors.New("private retirement intent missing")
		}
		old, err := m.multiInspect(ctx, j, slot.Current, false, true)
		if err != nil {
			return err
		}
		if old.State.Running || old.State.Restarting || old.HostConfig.RestartPolicy.Name != "no" {
			return errors.New("private retirement not proven")
		}
	}
	// A candidate can never be an effective/retired identity, even if a corrupt
	// caller supplies a valid name and labels.
	if multiEffectiveContainer(j, r.Container) {
		return errors.New("candidate aliases effective release")
	}
	if _, err := m.secretValues(j, r.SecretRefs); err != nil {
		return errors.New("read captured runtime secrets failed")
	}
	im, err := m.inspect(ctx, "image", r.Image)
	if err != nil {
		return errors.New("inspect candidate image failed")
	}
	if im.ID != r.Image || imageVolumesAllowed(im, r.Config, true) != nil {
		return errors.New("unsafe candidate image")
	}
	if err := m.ensureNetwork(ctx, j); err != nil {
		return errors.New("ensure app network failed")
	}
	exists, err := m.exists(ctx, "container", r.Container)
	if err != nil {
		return errors.New("list runtime containers failed")
	}
	if !exists {
		if r.Config.HTTP != nil {
			if r.Port != "" {
				return errors.New("recorded candidate endpoint missing")
			}
			if r.PublishPort == "" {
				allocate := m.AllocatePort
				if allocate == nil {
					allocate = allocatePort
				}
				port, err := allocate()
				if err != nil || !validRuntimePort(port) {
					return errors.New("allocate candidate port failed")
				}
				r.PublishPort = port
				if m.save(j) != nil {
					r.PublishPort = ""
					return errors.New("save candidate publish intent failed")
				}
			}
			if !validRuntimePort(r.PublishPort) {
				return errors.New("invalid candidate publish intent")
			}
		} else if r.Port != "" || r.PublishPort != "" {
			return errors.New("private service publishes ports")
		}
		args := []string{"run", "--detach", "--name", r.Container, "--restart", "unless-stopped", "--network", network(j)}
		if r.Config.Volume != nil {
			args = []string{"create", "--name", r.Container, "--restart", "no", "--network", network(j), "--mount", volumeMountArgument("soar-vol-"+r.VolumeID, r.Config.Volume.Mount)}
		}
		args = append(args, resourceArgs(r.Config.Resources)...)
		if r.Config.HTTP != nil {
			args = append(args, "--publish", fmt.Sprintf("127.0.0.1:%s:%d", r.PublishPort, r.Config.HTTP.Port))
		} else {
			args = append(args, "--network-alias", r.Service)
		}
		args = append(args, "--label", "soar.host="+j.HostID, "--label", "soar.app="+j.App, "--label", "soar.operation="+r.Operation, "--label", "soar.service="+r.Service)
		keys := make([]string, 0, len(r.Config.Env))
		for k := range r.Config.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--env", k+"="+r.Config.Env[k])
		}
		args = append(args, r.Image)
		args = append(args, r.Config.Command...)
		if m.runContainer(ctx, j, r, args) != nil {
			return errors.New("create candidate failed")
		}
	}
	v, err := m.multiInspect(ctx, j, r, false, r.Config.HTTP == nil)
	if err != nil {
		return err
	}
	if prepareOnly {
		if v.State.Running || v.State.Restarting || v.HostConfig.RestartPolicy.Name != "no" || r.ExecutionIntent {
			return errors.New("volume candidate executed before preparation completed")
		}
		return nil
	}
	if r.Config.Volume != nil {
		if err := m.checkVolumeWriters(ctx, j, r); err != nil {
			return err
		}
		if !r.ExecutionIntent {
			if v.State.Running || v.State.Restarting || v.HostConfig.RestartPolicy.Name != "no" {
				return errors.New("volume candidate execution was not captured")
			}
			r.ExecutionIntent = true
			slot.DataMayHaveChanged = true
			if err := m.save(j); err != nil {
				return err
			}
		}
		if v.HostConfig.RestartPolicy.Name == "no" {
			if _, err := m.cmd(ctx, "docker", "update", "--restart=unless-stopped", r.Container); err != nil {
				return err
			}
			v, err = m.multiInspect(ctx, j, r, false, r.Config.HTTP == nil)
			if err != nil {
				return err
			}
		}
	}
	if v.HostConfig.RestartPolicy.Name != "unless-stopped" {
		return errors.New("candidate retirement has begun")
	}
	if !v.State.Running {
		if _, err := m.cmd(ctx, "docker", "start", r.Container); err != nil {
			return errors.New("start candidate failed")
		}
		v, err = m.multiInspect(ctx, j, r, false, r.Config.HTTP == nil)
		if err != nil {
			return err
		}
	}
	if !runtimeRunning(v) {
		return errors.New("candidate is not running")
	}
	return nil
}

func runtimeRunning(v *inspection) bool {
	return v.State.Running && !v.State.Restarting && !v.State.Paused && !v.State.Dead
}

func (m *Manager) multiHealthy(ctx context.Context, j *journal, r *release) error {
	if err := multiIdentity(j, r); err != nil {
		return err
	}
	if r.Config.HTTP == nil {
		return errors.New("HTTP readiness requires public service")
	}
	ctx, cancel := context.WithTimeout(ctx, m.HealthTimeout)
	defer cancel()
	slot := j.ServiceStates[r.Service]
	initial := slot != nil && slot.Candidate != nil && slot.Candidate.Container == r.Container && (slot.Phase == "starting" || slot.Phase == "aliasing")
	requireAlias := r.Service != "" && !initial
	for {
		v, err := m.multiInspect(ctx, j, r, false, requireAlias)
		if err != nil {
			return err
		}
		if !runtimeRunning(v) || v.HostConfig.RestartPolicy.Name != "unless-stopped" {
			return errors.New("candidate is not running")
		}
		port := r.PublishPort
		if port == "" {
			port = r.Port
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+port+r.Config.HTTP.Health, nil)
		if err != nil {
			return errors.New("invalid readiness request")
		}
		resp, err := m.HTTP.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				// Reinspect after HTTP: a successful response cannot conceal changed ports
				// or a process that restarted while handling the request.
				after, err := m.multiInspect(ctx, j, r, false, requireAlias)
				if err != nil {
					return err
				}
				if !runtimeRunning(after) || after.HostConfig.RestartPolicy.Name != "unless-stopped" || after.RestartCount != v.RestartCount || after.State.StartedAt != v.State.StartedAt {
					return errors.New("candidate changed during readiness")
				}
				r.Port = port
				return nil
			}
		}
		if wait(ctx, 250*time.Millisecond) != nil {
			return errors.New("HTTP readiness failed")
		}
	}
}

// Docker network connect/disconnect use NETWORK CONTAINER (not container first).
// Explicit loopback publish bindings survive reconnect on the supported daemon.
// Endpoint IPs/connections may change; old/new readiness-qualified aliases can
// overlap briefly. No startup alias or DNS rollback guarantee is made.
func (m *Manager) multiAdmitAlias(ctx context.Context, j *journal, r *release) error {
	if !multiAliasing(j, r) {
		return errors.New("alias admission requires recorded intent")
	}
	v, err := m.multiInspect(ctx, j, r, true, false)
	if err != nil {
		return err
	}
	if len(v.NetworkSettings.Networks) == 0 {
		if _, err = m.cmd(ctx, "docker", "network", "connect", network(j), r.Container); err != nil {
			return errors.New("reconnect candidate for readiness failed")
		}
		v, err = m.multiInspect(ctx, j, r, false, false)
		if err != nil {
			return err
		}
	}
	if !hasAlias(v, network(j), r.Service) {
		if err = m.multiHealthy(ctx, j, r); err != nil {
			return err
		}
		if _, err = m.cmd(ctx, "docker", "network", "disconnect", network(j), r.Container); err != nil {
			return errors.New("disconnect candidate for alias failed")
		}
		// Inspect the detached object before reconnecting; never use a failed
		// command/inspect as evidence that an endpoint was actually removed.
		v, err = m.multiInspect(ctx, j, r, true, false)
		if err != nil {
			return err
		}
		if len(v.NetworkSettings.Networks) != 0 {
			return errors.New("candidate endpoint did not disconnect")
		}
		// A lost connect response may have admitted the endpoint. Always perform
		// post-readiness/withdrawal rather than returning with unverified DNS.
		_, _ = m.cmd(ctx, "docker", "network", "connect", "--alias", r.Service, network(j), r.Container)
	}
	if err = m.multiHealthy(ctx, j, r); err == nil {
		_, err = m.multiInspect(ctx, j, r, false, true)
	}
	if err == nil {
		return nil
	}
	// Post-admission failure must withdraw DNS exposure. Re-prove ownership
	// before mutation: a foreign replacement or configuration drift is not
	// authority to disconnect an endpoint. Never claim rollback in that case.
	beforeWithdrawal, verifyErr := m.inspect(ctx, "container", r.Container)
	// Compare with the fully verified pre-admission object. Effective port/IP
	// drift is precisely a reason to withdraw, not permission to ignore a
	// different container, labels, environment, or configured publish intent.
	if verifyErr != nil || beforeWithdrawal.ID != v.ID || beforeWithdrawal.Name != v.Name || !reflect.DeepEqual(beforeWithdrawal.Config, v.Config) || !reflect.DeepEqual(beforeWithdrawal.HostConfig, v.HostConfig) || !reflect.DeepEqual(beforeWithdrawal.Mounts, v.Mounts) {
		return errors.New("candidate readiness failed; endpoint withdrawal unconfirmed")
	}
	if len(beforeWithdrawal.NetworkSettings.Networks) == 0 {
		return errors.New("candidate readiness failed; endpoint withdrawn")
	}
	if len(beforeWithdrawal.NetworkSettings.Networks) != 1 || beforeWithdrawal.NetworkSettings.Networks[network(j)] == nil {
		return errors.New("candidate readiness failed; endpoint withdrawal unconfirmed")
	}
	// A lost disconnect response is not success; the next aliasing retry
	// reinspects the object.
	if _, withdrawErr := m.cmd(ctx, "docker", "network", "disconnect", network(j), r.Container); withdrawErr != nil {
		return errors.New("candidate readiness failed; endpoint withdrawal unconfirmed")
	}
	withdrawn, withdrawErr := m.multiInspect(ctx, j, r, true, false)
	if withdrawErr != nil || len(withdrawn.NetworkSettings.Networks) != 0 {
		return errors.New("candidate readiness failed; endpoint withdrawal unconfirmed")
	}
	return errors.New("candidate readiness failed; endpoint withdrawn")
}

func (m *Manager) multiObserve(ctx context.Context, j *journal, r *release) error {
	if err := multiIdentity(j, r); err != nil {
		return err
	}
	if r.Config.HTTP != nil {
		return errors.New("private observation requires private service")
	}
	first, err := m.multiInspect(ctx, j, r, false, true)
	if err != nil {
		return err
	}
	if !runtimeRunning(first) || first.State.StartedAt == "" || first.HostConfig.RestartPolicy.Name != "unless-stopped" {
		return errors.New("private candidate is not running")
	}
	deadline := time.Now().Add(m.PrivateObserve)
	for {
		remaining := time.Until(deadline)
		if remaining > 0 {
			if remaining > 100*time.Millisecond {
				remaining = 100 * time.Millisecond
			}
			if wait(ctx, remaining) != nil {
				return errors.New("private observation interrupted")
			}
		}
		v, err := m.multiInspect(ctx, j, r, false, true)
		if err != nil {
			return err
		}
		if !runtimeRunning(v) || v.HostConfig.RestartPolicy.Name != "unless-stopped" || v.RestartCount != first.RestartCount || v.State.StartedAt != first.State.StartedAt {
			return errors.New("private candidate restarted or stopped during observation")
		}
		if !time.Now().Before(deadline) {
			return nil
		}
	}
}

func (m *Manager) multiRetire(ctx context.Context, j *journal, r *release) error {
	return m.multiStop(ctx, j, r, false, true)
}

func (m *Manager) multiStop(ctx context.Context, j *journal, r *release, allowDetached, requireAlias bool) error {
	v, err := m.multiInspect(ctx, j, r, allowDetached, requireAlias)
	if err != nil {
		return err
	}
	if v.HostConfig.RestartPolicy.Name != "no" {
		if _, err = m.cmd(ctx, "docker", "update", "--restart=no", r.Container); err != nil {
			return errors.New("disable runtime restart failed")
		}
		v, err = m.multiInspect(ctx, j, r, allowDetached, requireAlias)
		if err != nil {
			return err
		}
		if v.HostConfig.RestartPolicy.Name != "no" {
			return errors.New("runtime restart remains enabled")
		}
	}
	if v.State.Running || v.State.Restarting {
		if _, err = m.cmd(ctx, "docker", "stop", "--time", "30", r.Container); err != nil {
			return errors.New("stop runtime container failed")
		}
	}
	v, err = m.multiInspect(ctx, j, r, allowDetached, requireAlias)
	if err != nil {
		return err
	}
	if v.State.Running || v.State.Restarting || v.State.Paused || v.HostConfig.RestartPolicy.Name != "no" {
		return errors.New("runtime retirement not proven")
	}
	return nil
}

func multiEffectiveContainer(j *journal, name string) bool {
	for _, r := range []*release{j.Current, j.Previous} {
		if r != nil && r.Container == name {
			return true
		}
	}
	for _, s := range j.ServiceStates {
		if s != nil {
			for _, r := range []*release{s.Current, s.Retiring} {
				if r != nil && r.Container == name {
					return true
				}
			}
		}
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil {
			for _, r := range snapshot.Services {
				if r != nil && r.Container == name {
					return true
				}
			}
		}
	}
	return false
}

func (m *Manager) multiDiscard(ctx context.Context, j *journal, r *release, allowDetached bool) error {
	if err := multiIdentity(j, r); err != nil {
		return err
	}
	if allowDetached && !multiAliasing(j, r) {
		return errors.New("detached cleanup requires aliasing intent")
	}
	slot := j.ServiceStates[r.Service]
	if slot == nil || slot.Candidate == nil || slot.Candidate.Container != r.Container || slot.Candidate.Operation != r.Operation {
		return errors.New("discard requires recorded candidate")
	}
	if multiEffectiveContainer(j, r.Container) {
		return errors.New("cannot discard effective release")
	}
	exists, err := m.exists(ctx, "container", r.Container)
	if err != nil {
		return errors.New("list discard candidate failed")
	}
	if !exists {
		return nil
	}
	// Public candidates before admission correctly have no service alias.
	if err = m.multiStop(ctx, j, r, allowDetached, r.Config.HTTP == nil); err != nil {
		return err
	}
	if _, err = m.cmd(ctx, "docker", "rm", r.Container); err != nil {
		return errors.New("remove candidate failed")
	}
	exists, err = m.exists(ctx, "container", r.Container)
	if err != nil || exists {
		return errors.New("candidate removal not proven")
	}
	return nil
}
