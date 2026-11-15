package buildkit

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"time"
)

type inventoryEntry struct{ ID, Name, Names string }

func (b *build) inventory(ctx context.Context, kind, name string) (string, error) {
	args := []string{kind, "ls"}
	if kind == "container" {
		args = append(args, "--all")
	}
	if kind != "volume" {
		args = append(args, "--no-trunc")
	}
	args = append(args, "--format", "{{json .}}")
	r, err := b.docker(ctx, args...)
	if err != nil {
		return "", errInfra
	}
	found := ""
	for _, line := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
		if line == "" {
			continue
		}
		var e inventoryEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			return "", errInfra
		}
		n := e.Name
		if kind == "container" {
			n = e.Names
		}
		if n == "" {
			return "", errInfra
		}
		id := e.ID
		if kind == "volume" {
			id = n
		} else if !idPattern.MatchString(id) {
			return "", errInfra
		}
		if n == name {
			if found != "" {
				return "", errInfra
			}
			found = id
		}
	}
	return found, nil
}
func (b *build) inspect(ctx context.Context, kind, id string, out any) error {
	r, err := b.docker(ctx, kind, "inspect", id)
	if err != nil {
		return errInfra
	}
	var list []json.RawMessage
	if json.Unmarshal(r.Stdout, &list) != nil || len(list) != 1 || json.Unmarshal(list[0], out) != nil {
		return errInfra
	}
	return nil
}
func labelArgs(labels map[string]string) []string {
	// Stable order also makes runner traces useful in regression tests.
	var args []string
	for _, key := range []string{"org.vamos.buildkit.host", "org.vamos.buildkit.nonce", "org.vamos.buildkit.role"} {
		args = append(args, "--label", key+"="+labels[key])
	}
	return args
}

type ipamConfig struct {
	Subnet, IPRange, Gateway string
	AuxiliaryAddresses       map[string]string
}
type ipam struct {
	Driver  string
	Options map[string]string
	Config  []ipamConfig
}
type networkInfo struct {
	ID, Name, Driver, Scope                               string
	EnableIPv6, Internal, Attachable, Ingress, ConfigOnly bool
	EnableIPv4                                            *bool
	ConfigFrom                                            struct{ Network string }
	IPAM                                                  ipam
	Options, Labels                                       map[string]string
	Containers                                            map[string]struct{ Name string }
}

func (b *build) network(ctx context.Context) error {
	s := &b.s
	id, err := b.inventory(ctx, "network", s.Network)
	if err != nil {
		return err
	}
	if s.NetworkID != "" && s.NetworkID != id {
		return errInfra
	}
	if id == "" {
		args := []string{"network", "create", "--driver", "bridge", "--ipv6=false", "--opt", "com.docker.network.bridge.name=" + bridge, "--opt", "com.docker.network.bridge.enable_icc=false"}
		args = append(args, labelArgs(s.labels("network"))...)
		args = append(args, s.Network)
		if _, err = b.docker(ctx, args...); err != nil {
			return errInfra
		}
		id, err = b.inventory(ctx, "network", s.Network)
		if err != nil || id == "" {
			return errInfra
		}
	}
	var n networkInfo
	if b.inspect(ctx, "network", id, &n) != nil {
		return errInfra
	}
	wantOpts := map[string]string{"com.docker.network.bridge.name": bridge, "com.docker.network.bridge.enable_icc": "false"}
	if n.ID != id || n.Name != s.Network || n.Driver != "bridge" || n.Scope != "local" || n.EnableIPv6 || n.Internal || n.Attachable || n.Ingress || n.ConfigOnly || n.ConfigFrom.Network != "" || (n.EnableIPv4 != nil && !*n.EnableIPv4) || !reflect.DeepEqual(n.Options, wantOpts) || !reflect.DeepEqual(n.Labels, s.labels("network")) {
		return errInfra
	}
	if n.IPAM.Driver != "default" || len(n.IPAM.Options) != 0 || len(n.IPAM.Config) != 1 {
		return errInfra
	}
	c := n.IPAM.Config[0]
	ip, subnet, err := net.ParseCIDR(c.Subnet)
	gw := net.ParseIP(c.Gateway)
	if err != nil || ip.To4() == nil || gw.To4() == nil || !subnet.Contains(gw) || c.IPRange != "" || len(c.AuxiliaryAddresses) != 0 {
		return errInfra
	}
	for cid, endpoint := range n.Containers {
		if cid != s.ContainerID || endpoint.Name != s.Container {
			return errInfra
		}
	}
	if s.NetworkID != "" && !reflect.DeepEqual(s.IPAM, n.IPAM) {
		return errInfra
	}
	s.NetworkID = id
	s.IPAM = n.IPAM
	return save(b.dir, *s)
}

type volumeInfo struct {
	Name, Driver, Scope, CreatedAt, Mountpoint string
	Labels, Options                            map[string]string
}

func (b *build) volume(ctx context.Context) error {
	s := &b.s
	name, err := b.inventory(ctx, "volume", s.Volume)
	if err != nil {
		return err
	}
	if s.VolumeCreated != "" && name == "" {
		return errInfra
	}
	if name == "" {
		args := []string{"volume", "create", "--driver", "local"}
		args = append(args, labelArgs(s.labels("volume"))...)
		args = append(args, s.Volume)
		if _, err = b.docker(ctx, args...); err != nil {
			return errInfra
		}
		name, err = b.inventory(ctx, "volume", s.Volume)
		if err != nil || name == "" {
			return errInfra
		}
	}
	var v volumeInfo
	if b.inspect(ctx, "volume", name, &v) != nil {
		return errInfra
	}
	if v.Name != s.Volume || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 || !reflect.DeepEqual(v.Labels, s.labels("volume")) || !strings.HasPrefix(v.Mountpoint, "/") {
		return errInfra
	}
	if _, err = time.Parse(time.RFC3339Nano, v.CreatedAt); err != nil {
		return errInfra
	}
	if s.VolumeCreated != "" && (v.CreatedAt != s.VolumeCreated || v.Mountpoint != s.VolumeMountpoint) {
		return errInfra
	}
	s.VolumeCreated = v.CreatedAt
	s.VolumeMountpoint = v.Mountpoint
	return save(b.dir, *s)
}
func (b *build) image(ctx context.Context) error {
	if b.s.ImageID != "" {
		return nil
	}
	if _, err := b.docker(ctx, "image", "pull", Image); err != nil {
		return errInfra
	}
	var im struct {
		ID          string
		RepoDigests []string
		Config      struct {
			Env    []string
			Labels map[string]string
		}
	}
	if b.inspect(ctx, "image", Image, &im) != nil || !strings.HasPrefix(im.ID, "sha256:") || !idPattern.MatchString(strings.TrimPrefix(im.ID, "sha256:")) {
		return errInfra
	}
	digest := "moby/buildkit@" + strings.Split(Image, "@")[1]
	found := false
	for _, d := range im.RepoDigests {
		if d == digest {
			found = true
		}
	}
	if !found {
		return errInfra
	}
	b.s.ImageID = im.ID
	b.s.ImageEnv = im.Config.Env
	b.s.ImageLabels = im.Config.Labels
	return save(b.dir, b.s)
}

type containerInfo struct {
	ID, Name, Image, Path string
	Args                  []string
	Config                json.RawMessage
	HostConfig            json.RawMessage
	State                 struct{ Running, Paused, Restarting bool }
	Mounts                []struct {
		Type, Name, Source, Destination, Driver, Mode, Propagation string
		RW                                                         bool
	}
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID      string
			Aliases, Links []string
			IPAMConfig     json.RawMessage
			DriverOpts     map[string]string
		}
	}
}
type containerConfig struct {
	MacAddress                                                         string
	NetworkDisabled                                                    bool
	Image, User, WorkingDir                                            string
	Entrypoint, Cmd, Env, OnBuild, Shell                               []string
	Labels                                                             map[string]string
	ExposedPorts                                                       map[string]json.RawMessage
	Volumes                                                            map[string]json.RawMessage
	Healthcheck                                                        json.RawMessage
	Tty, OpenStdin, StdinOnce, AttachStdin, AttachStdout, AttachStderr bool
}
type containerHost struct {
	Runtime, Isolation      string
	Init, OomKillDisable    *bool
	DeviceCgroupRules       []string
	StorageOpt, Annotations map[string]string
	LogConfig               struct {
		Type   string
		Config map[string]string
	}
	Privileged, AutoRemove, PublishAllPorts, ReadonlyRootfs                           bool
	NetworkMode, PidMode, IpcMode, UTSMode, UsernsMode, CgroupParent, ContainerIDFile string
	RestartPolicy                                                                     struct {
		Name              string
		MaximumRetryCount int
	}
	Binds, CapAdd, CapDrop, SecurityOpt, VolumesFrom, Links, ExtraHosts, DNS, DNSOptions, DNSSearch, GroupAdd []string
	PortBindings, Tmpfs, Sysctls                                                                              map[string]json.RawMessage
	Devices, DeviceRequests                                                                                   []json.RawMessage
	Mounts                                                                                                    []struct {
		Type, Source, Target                     string
		ReadOnly                                 bool
		Consistency                              string
		BindOptions, VolumeOptions, TmpfsOptions json.RawMessage
	}
}

func emptyJSON(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }
func (b *build) validateContainer(c containerInfo, id string) error {
	s := b.s
	var cfg containerConfig
	var h containerHost
	if json.Unmarshal(c.Config, &cfg) != nil || json.Unmarshal(c.HostConfig, &h) != nil {
		return errInfra
	}
	labels := map[string]string{}
	for k, v := range s.ImageLabels {
		labels[k] = v
	}
	for k, v := range s.labels("container") {
		labels[k] = v
	}
	cmd := []string{"--oci-worker-snapshotter=" + s.Snapshotter}
	if c.ID != id || !idPattern.MatchString(id) || c.Name != "/"+s.Container || c.Image != s.ImageID || c.Path != "buildkitd" || !reflect.DeepEqual(c.Args, cmd) || cfg.Image != Image || cfg.User != "0:0" || cfg.WorkingDir != "/" || !reflect.DeepEqual(cfg.Entrypoint, []string{"buildkitd"}) || !reflect.DeepEqual(cfg.Cmd, cmd) || !validBuilderEnv(cfg.Env, s.ImageEnv) || !reflect.DeepEqual(cfg.Labels, labels) || len(cfg.ExposedPorts) != 0 || len(cfg.OnBuild) != 0 || len(cfg.Shell) != 0 || !emptyJSON(cfg.Healthcheck) || cfg.Tty || cfg.OpenStdin || cfg.StdinOnce || cfg.AttachStdin || !cfg.AttachStdout || !cfg.AttachStderr {
		return errInfra
	}
	for dest := range cfg.Volumes {
		if dest != "/var/lib/buildkit" {
			return errInfra
		}
	}
	if cfg.MacAddress != "" || cfg.NetworkDisabled || (h.Runtime != "" && h.Runtime != "runc") || (h.Isolation != "" && h.Isolation != "default") || (h.Init != nil && *h.Init) || (h.OomKillDisable != nil && *h.OomKillDisable) || len(h.DeviceCgroupRules)+len(h.StorageOpt)+len(h.Annotations) != 0 || h.LogConfig.Type != "local" || len(h.LogConfig.Config) != 0 {
		return errInfra
	}
	if !h.Privileged || h.AutoRemove || h.PublishAllPorts || h.ReadonlyRootfs || h.NetworkMode != s.NetworkID || h.RestartPolicy.Name != "no" || h.RestartPolicy.MaximumRetryCount != 0 || h.PidMode != "" || (h.IpcMode != "private" && h.IpcMode != "") || h.UTSMode != "" || h.UsernsMode != "" || h.CgroupParent != "" || h.ContainerIDFile != "" {
		return errInfra
	}
	if len(h.Binds)+len(h.CapAdd)+len(h.CapDrop)+len(h.VolumesFrom)+len(h.Links)+len(h.ExtraHosts)+len(h.DNS)+len(h.DNSOptions)+len(h.DNSSearch)+len(h.GroupAdd)+len(h.PortBindings)+len(h.Tmpfs)+len(h.Sysctls)+len(h.Devices)+len(h.DeviceRequests) != 0 || len(h.Mounts) != 1 {
		return errInfra
	}
	if !reflect.DeepEqual(h.SecurityOpt, []string{"label=disable"}) {
		return errInfra
	}
	hm := h.Mounts[0]
	if hm.Type != "volume" || hm.Source != s.Volume || hm.Target != "/var/lib/buildkit" || hm.ReadOnly || hm.Consistency != "" || !emptyJSON(hm.BindOptions) || !emptyJSON(hm.VolumeOptions) || !emptyJSON(hm.TmpfsOptions) {
		return errInfra
	}
	if len(c.Mounts) != 1 {
		return errInfra
	}
	mount := c.Mounts[0]
	if mount.Type != "volume" || mount.Name != s.Volume || mount.Source != s.VolumeMountpoint || mount.Destination != "/var/lib/buildkit" || mount.Driver != "local" || !mount.RW || (mount.Mode != "" && mount.Mode != "z") || mount.Propagation != "" {
		return errInfra
	}
	// Docker retains the configured network entry while a container is stopped.
	if len(c.NetworkSettings.Networks) != 1 {
		return errInfra
	}
	for name, n := range c.NetworkSettings.Networks {
		if (name != s.Network && (c.State.Running || name != s.NetworkID)) || (n.NetworkID != s.NetworkID && (c.State.Running || n.NetworkID != "")) || len(n.Links) != 0 || len(n.DriverOpts) != 0 || !emptyJSON(n.IPAMConfig) {
			return errInfra
		}
		for _, a := range n.Aliases {
			if a != s.Container && a != id[:12] {
				return errInfra
			}
		}
	}
	if s.ContainerID != "" && (s.ContainerID != id || !sameJSON(s.ContainerConfig, c.Config) || !sameContainerHost(s.ContainerHost, c.HostConfig)) {
		return errInfra
	}
	return nil
}

// Docker 29 normalizes default OOM policy from false to null at first start,
// and empty DNS from null to [] after a daemon restart. Both settings are
// independently constrained to their defaults in validateContainer above.
func sameContainerHost(a, b []byte) bool {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	for _, host := range []map[string]json.RawMessage{left, right} {
		if value := host["OomKillDisable"]; emptyJSON(value) || string(value) == "false" {
			delete(host, "OomKillDisable")
		}
		if value := host["Dns"]; emptyJSON(value) || string(value) == "[]" {
			delete(host, "Dns")
		}
	}
	x, _ := json.Marshal(left)
	y, _ := json.Marshal(right)
	return sameJSON(x, y)
}

// Docker CLI may inject these proxy variables from root's trusted config.json
// during create. Capture them in ContainerConfig, then require exact equality on
// every reuse; arbitrary extra variables and changes still fail closed.
func validBuilderEnv(actual, image []string) bool {
	expected := make(map[string]string, len(image))
	for _, entry := range image {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return false
		}
		expected[key] = value
	}
	seen := make(map[string]bool, len(actual))
	for _, entry := range actual {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		if want, exists := expected[key]; exists {
			if value != want {
				return false
			}
			delete(expected, key)
			continue
		}
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy":
		default:
			return false
		}
	}
	return len(expected) == 0
}

func (b *build) existingContainer(ctx context.Context) error {
	id, err := b.inventory(ctx, "container", b.s.Container)
	if err != nil {
		return err
	}
	if b.s.ContainerID != "" && id != b.s.ContainerID {
		return errInfra
	}
	if id == "" {
		return nil
	}
	var c containerInfo
	if b.inspect(ctx, "container", id, &c) != nil || b.validateContainer(c, id) != nil {
		return errInfra
	}
	// Register cleanup before persisting anything: failed disk writes must not
	// leave an already-running, verified daemon behind.
	b.stopID = id
	if b.stop() != nil {
		return errInfra
	}
	b.s.ContainerID = id
	b.s.ContainerConfig = c.Config
	b.s.ContainerHost = c.HostConfig
	return save(b.dir, b.s)
}
func (b *build) createContainer(ctx context.Context) error {
	if b.stopID != "" {
		return nil
	}
	s := b.s
	args := []string{"container", "create", "--name", s.Container, "--privileged", "--security-opt", "label=disable", "--attach", "stdout", "--attach", "stderr", "--ipc", "private", "--log-driver", "local", "--restart=no", "--network", s.NetworkID, "--user", "0:0", "--workdir", "/", "--entrypoint", "buildkitd", "--mount", "type=volume,source=" + s.Volume + ",target=/var/lib/buildkit"}
	args = append(args, labelArgs(s.labels("container"))...)
	args = append(args, Image, "--oci-worker-snapshotter="+s.Snapshotter)
	if _, err := b.docker(ctx, args...); err != nil {
		return errInfra
	}
	return b.existingContainer(ctx)
}
