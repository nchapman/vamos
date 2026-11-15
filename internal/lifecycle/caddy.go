package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Caddy runs as a Vamos-managed container pinned by digest, so Vamos controls
// the ingress version independently of the distribution's archive. Upgrading
// Vamos is how Caddy is upgraded.
const (
	caddyDigest = "sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c"
	// CaddyImage keeps the tag for readability; Docker resolves the digest.
	CaddyImage = "caddy:2.11.9@" + caddyDigest
	// caddyImageRef is the form Docker's image inventory and removal accept.
	caddyImageRef  = "caddy@" + caddyDigest
	CaddyContainer = "vamos-caddy"
	caddyHostLabel = "org.vamos.caddy.host"
	// Host paths. /etc/caddy is mounted read-only at the same path so callers
	// can pass identical paths to the in-container CLI.
	caddyfilePath   = "/etc/caddy/Caddyfile"
	caddyDataDir    = "/var/lib/caddy/data"
	caddyConfigDir  = "/var/lib/caddy/config"
	caddyRestart    = "unless-stopped"
	caddyNetworking = "host"
)

// CaddyCommand returns the invocation that runs the Caddy CLI inside the managed
// ingress container. Paths under /etc/caddy are valid unchanged.
func CaddyCommand(args ...string) (string, []string) {
	return "docker", append([]string{"exec", CaddyContainer, "caddy"}, args...)
}

// caddyRunArgs creates the ingress container. Host networking keeps loopback
// upstreams, the loopback admin API, HTTP/3 and client addresses unchanged. The
// root user keeps only the capability to bind 80/443, on a read-only rootfs.
// unless-stopped brings Caddy back after Docker restarts, including the daemon
// restarts that Ubuntu runtime package updates trigger.
func caddyRunArgs(hostID string) []string {
	return []string{
		"run", "--detach", "--name", CaddyContainer,
		"--label", caddyHostLabel + "=" + hostID,
		"--restart", caddyRestart, "--network", caddyNetworking, "--log-driver", "local",
		"--user", "0:0", "--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--security-opt", "no-new-privileges",
		"--mount", "type=bind,source=/etc/caddy,target=/etc/caddy,readonly",
		"--mount", "type=bind,source=" + caddyDataDir + ",target=/data",
		"--mount", "type=bind,source=" + caddyConfigDir + ",target=/config",
		CaddyImage, "caddy", "run", "--config", caddyfilePath, "--adapter", "caddyfile",
	}
}

// caddyContainer holds only the inspect fields Vamos sets itself, so Docker
// updates that add or change unrelated defaults do not register as drift.
type caddyContainer struct {
	ID     string `json:"Id"`
	Config struct {
		Image      string            `json:"Image"`
		User       string            `json:"User"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	// GraphDriver reports the root filesystem path only for Docker's classic
	// storage drivers; the containerd image store mounts it under a path named
	// by the container ID instead.
	GraphDriver struct {
		Data struct {
			MergedDir string `json:"MergedDir"`
		} `json:"Data"`
	} `json:"GraphDriver"`
	HostConfig struct {
		NetworkMode   string `json:"NetworkMode"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
		LogConfig struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
		Privileged     bool              `json:"Privileged"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		CapAdd         []string          `json:"CapAdd"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		Binds          []string          `json:"Binds"`
		Mounts         []struct {
			Type     string `json:"Type"`
			Source   string `json:"Source"`
			Target   string `json:"Target"`
			ReadOnly bool   `json:"ReadOnly"`
		} `json:"Mounts"`
	} `json:"HostConfig"`
}

// ownsMount reports whether a host mount point is this container's root
// filesystem: the containerd image store mounts it at
// /var/lib/docker/rootfs/<snapshotter>/<container ID>, while classic storage
// drivers report it as the merged directory.
func (c *caddyContainer) ownsMount(mount string) bool {
	if c.GraphDriver.Data.MergedDir != "" && mount == c.GraphDriver.Data.MergedDir {
		return true
	}
	rest, ok := strings.CutPrefix(mount, "/var/lib/docker/rootfs/")
	if !ok {
		return false
	}
	snapshotter, id, ok := strings.Cut(rest, "/")
	return ok && snapshotter != "" && !strings.Contains(snapshotter, ".") && id == c.ID
}

// ownsContainerd reports whether a containerd resource in Docker's moby
// namespace is this container or its pinned image.
func (c *caddyContainer) ownsContainerd(resource, item string) bool {
	switch resource {
	case "containers":
		return item == c.ID
	case "images":
		name, digest, ok := strings.Cut(item, "@")
		if !ok || (name != "docker.io/library/caddy" && !strings.HasPrefix(name, "docker.io/library/caddy:")) {
			return false
		}
		for _, ref := range c.imageRefs() {
			if strings.HasSuffix(ref, "@"+digest) {
				return true
			}
		}
	}
	return false
}

// imageRefs are the owned ingress images teardown may find: the pinned image
// and, before server upgrade replaces the container, the image it still runs.
func (c *caddyContainer) imageRefs() []string {
	if ref := c.imageRef(); ref != caddyImageRef {
		return []string{caddyImageRef, ref}
	}
	return []string{caddyImageRef}
}

// imageRef is the digest reference of the image this container runs, which
// differs from the pinned image until server upgrade replaces the container.
func (c *caddyContainer) imageRef() string {
	if ref := digestRef(c.Config.Image); ref != "" {
		return ref
	}
	return caddyImageRef
}

func (c *caddyContainer) current() bool {
	if c.Config.Image != CaddyImage || c.Config.User != "0:0" || len(c.Config.Entrypoint) != 0 ||
		!slices.Equal(c.Config.Cmd, []string{"caddy", "run", "--config", caddyfilePath, "--adapter", "caddyfile"}) {
		return false
	}
	h := c.HostConfig
	if h.NetworkMode != caddyNetworking || h.RestartPolicy.Name != caddyRestart || h.LogConfig.Type != "local" ||
		h.Privileged || !h.ReadonlyRootfs || len(h.Binds) != 0 ||
		!slices.Equal(h.CapDrop, []string{"ALL"}) ||
		(!slices.Equal(h.CapAdd, []string{"CAP_NET_BIND_SERVICE"}) && !slices.Equal(h.CapAdd, []string{"NET_BIND_SERVICE"})) ||
		!slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}) ||
		len(h.Tmpfs) != 1 || h.Tmpfs["/tmp"] != "" || len(h.Mounts) != 3 {
		return false
	}
	want := [3]struct {
		source, target string
		readOnly       bool
	}{
		{"/etc/caddy", "/etc/caddy", true},
		{caddyDataDir, "/data", false},
		{caddyConfigDir, "/config", false},
	}
	for i, m := range h.Mounts {
		if m.Type != "bind" || m.Source != want[i].source || m.Target != want[i].target || m.ReadOnly != want[i].readOnly {
			return false
		}
	}
	return true
}

// findCaddy returns the container named vamos-caddy, or nil when a successful
// inventory proves it absent. A container with that name that does not carry
// this installation's label is never adopted or removed.
func (m *Manager) findCaddy(ctx context.Context, s *State) (*caddyContainer, error) {
	out, err := m.command(ctx, "docker", "container", "ls", "--all", "--no-trunc", "--filter", "name=^"+CaddyContainer+"$", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) != 1 {
		return nil, errors.New("ambiguous Caddy container inventory")
	}
	data, err := m.command(ctx, "docker", "container", "inspect", "--format", "{{json .}}", ids[0])
	if err != nil {
		return nil, err
	}
	var c caddyContainer
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("unrecognized Caddy container inspection: %w", err)
	}
	if c.ID != ids[0] {
		return nil, errors.New("Caddy container identity changed during inspection")
	}
	if c.Config.Labels[caddyHostLabel] != s.ID {
		return nil, fmt.Errorf("container %s is not owned by this installation; refusing to adopt or replace it", CaddyContainer)
	}
	return &c, nil
}

// ensureCaddy converges the ingress container: create it when absent, replace
// it when its image or shape differs from this release, and start it when
// stopped. Routes and certificates live on the host, so replacement keeps them.
func (m *Manager) ensureCaddy(ctx context.Context, s *State) error {
	c, err := m.findCaddy(ctx, s)
	if err != nil {
		return err
	}
	if c != nil && !c.current() {
		// Pull first so a slow or failed download never leaves ingress down.
		if _, err := m.command(ctx, "docker", "pull", "--quiet", CaddyImage); err != nil {
			return err
		}
		if _, err := m.command(ctx, "docker", "container", "rm", "--force", c.ID); err != nil {
			return err
		}
		previous := digestRef(c.Config.Image)
		c = nil
		defer func() {
			// Best effort: an image still used elsewhere is retained by Docker.
			if previous != "" && previous != caddyImageRef {
				_, _ = m.Run(ctx, "docker", []string{"image", "rm", previous})
			}
		}()
	}
	if c == nil {
		for _, dir := range []string{caddyDataDir, caddyConfigDir} {
			if err := rejectSymlink(m.path(dir)); err != nil {
				return err
			}
			if err := os.MkdirAll(m.path(dir), 0700); err != nil {
				return err
			}
		}
		if _, err := m.command(ctx, "docker", "pull", "--quiet", CaddyImage); err != nil {
			return err
		}
		if _, err := m.command(ctx, "docker", caddyRunArgs(s.ID)...); err != nil {
			return err
		}
		if c, err = m.findCaddy(ctx, s); err != nil {
			return err
		}
		if c == nil {
			return errors.New("Caddy container is missing after creation")
		}
	} else if !c.State.Running {
		if _, err := m.command(ctx, "docker", "container", "start", c.ID); err != nil {
			return err
		}
	}
	if s.CaddyContainerID != c.ID {
		s.CaddyContainerID = c.ID
		return m.save(s)
	}
	return nil
}

// checkCaddy verifies the running ingress container matches this release.
func (m *Manager) checkCaddy(ctx context.Context, s *State) error {
	c, err := m.findCaddy(ctx, s)
	if err != nil {
		return err
	}
	if c == nil || c.ID != s.CaddyContainerID {
		return errors.New("managed Caddy container is missing or was replaced; run vamos server upgrade to repair it")
	}
	if !c.current() {
		return errors.New("managed Caddy container does not match this release; run vamos server upgrade")
	}
	if !c.State.Running {
		return errors.New("managed Caddy container is not running; run vamos server upgrade to restart it")
	}
	return nil
}

// removeCaddy deletes the owned ingress container, the image it ran and the
// pinned image during an approved teardown. Successful inventories establish
// absence.
func (m *Manager) removeCaddy(ctx context.Context, s *State) error {
	c, err := m.findCaddy(ctx, s)
	if err != nil {
		return err
	}
	refs := []string{caddyImageRef}
	if c != nil {
		refs = c.imageRefs()
		if _, err := m.command(ctx, "docker", "container", "rm", "--force", c.ID); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		images, err := m.caddyImageIDs(ctx, ref)
		if err != nil {
			return err
		}
		if len(images) != 0 {
			if _, err := m.command(ctx, "docker", "image", "rm", ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// caddyImageIDs lists an image's IDs; empty output proves it absent.
func (m *Manager) caddyImageIDs(ctx context.Context, ref string) ([]string, error) {
	out, err := m.command(ctx, "docker", "image", "ls", "--quiet", "--no-trunc", ref)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// digestRef reduces "name:tag@digest" to "name@digest", or "" without a digest.
func digestRef(ref string) string {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ""
	}
	if slash := strings.LastIndex(name, "/"); strings.LastIndex(name, ":") > slash {
		name = name[:strings.LastIndex(name, ":")]
	}
	return name + "@" + digest
}
