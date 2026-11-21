package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	caddyRepo   = "ghcr.io/nchapman/vamos/caddy"
	CaddyImage  = caddyRepo + ":20261120"
	caddyDigest = "sha256:b6a94eb93915955ea6df57a16dd69b80274fc21b188713d998261b49e4975eea"
	caddyPinned = caddyRepo + ":2.11.9@" + caddyDigest
	// caddyImageRef is the form Docker's image inventory and removal accept.
	caddyImageRef  = caddyRepo + "@" + caddyDigest
	CaddyContainer = "vamos-caddy"
	caddyHostLabel = "org.vamos.caddy.host"
	// Host paths and loopback admin endpoint shared with app deployments.
	// /etc/caddy is mounted read-only at the same path so callers can pass
	// identical paths to the in-container CLI.
	CaddyfilePath   = "/etc/caddy/Caddyfile"
	CaddyAdminURL   = "http://127.0.0.1:2019/config/"
	caddyfilePath   = CaddyfilePath
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
func caddyRunArgs(hostID, image string) []string {
	return []string{
		"run", "--pull=never", "--detach", "--name", CaddyContainer,
		"--label", caddyHostLabel + "=" + hostID,
		"--restart", caddyRestart, "--network", caddyNetworking, "--log-driver", "local",
		"--user", "0:0", "--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--security-opt", "no-new-privileges",
		"--mount", "type=bind,source=/etc/caddy,target=/etc/caddy,readonly",
		"--mount", "type=bind,source=" + caddyDataDir + ",target=/data",
		"--mount", "type=bind,source=" + caddyConfigDir + ",target=/config",
		image, "caddy", "run", "--config", caddyfilePath, "--adapter", "caddyfile",
	}
}

// caddyContainer holds only the inspect fields Vamos sets itself, so Docker
// updates that add or change unrelated defaults do not register as drift.
type caddyContainer struct {
	ID        string `json:"Id"`
	Image     string `json:"Image"`
	ownsTag   bool
	tagDigest string
	Config    struct {
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
		if item == CaddyImage {
			return c.ownsTag
		}
		ref := digestRef(item)
		if strings.HasPrefix(ref, "docker.io/library/caddy@") {
			ref = strings.TrimPrefix(ref, "docker.io/library/")
		}
		return ref != "" && slices.Contains(c.imageRefs(), ref)
	}
	return false
}

// imageRefs are the owned ingress images teardown may find: the pinned image
// and, before server upgrade replaces the container, the image it still runs.
func (c *caddyContainer) imageRefs() []string {
	refs := []string{caddyImageRef}
	if c.ownsTag {
		refs = append([]string{CaddyImage}, refs...)
	}
	if ref := c.imageRef(); ref != caddyImageRef {
		refs = append(refs, ref)
	}
	if c.tagDigest != "" && !slices.Contains(refs, c.tagDigest) {
		refs = append(refs, c.tagDigest)
	}
	return refs
}

// imageRef is the digest reference of the image this container runs, which
// differs from the pinned image until server upgrade replaces the container.
func (c *caddyContainer) imageRef() string {
	if ref := ownedCaddyDigest(c.Config.Image); ref != "" {
		return ref
	}
	return caddyImageRef
}

// Older installations also used the official Docker Hub Caddy image.
func ownedCaddyDigest(image string) string {
	ref := digestRef(image)
	repo, digest, _ := strings.Cut(ref, "@")
	if (repo == caddyRepo || repo == "caddy" || repo == "docker.io/library/caddy") && imageIdentity.MatchString(digest) {
		return ref
	}
	return ""
}

func (c *caddyContainer) current() bool {
	lineDigest, line := strings.CutPrefix(c.Config.Image, CaddyImage+"@")
	if !imageIdentity.MatchString(c.Image) || (!(line && imageIdentity.MatchString(lineDigest)) && c.Config.Image != caddyPinned && c.Config.Image != caddyImageRef) || c.Config.User != "0:0" || len(c.Config.Entrypoint) != 0 ||
		!slices.Equal(c.Config.Cmd, []string{"caddy", "run", "--config", caddyfilePath, "--adapter", "caddyfile"}) {
		return false
	}
	h := c.HostConfig
	_, tmp := h.Tmpfs["/tmp"]
	if h.NetworkMode != caddyNetworking || h.RestartPolicy.Name != caddyRestart || h.LogConfig.Type != "local" ||
		h.Privileged || !h.ReadonlyRootfs || len(h.Binds) != 0 ||
		!slices.Equal(h.CapDrop, []string{"ALL"}) ||
		(!slices.Equal(h.CapAdd, []string{"CAP_NET_BIND_SERVICE"}) && !slices.Equal(h.CapAdd, []string{"NET_BIND_SERVICE"})) ||
		!slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}) ||
		!tmp || len(h.Tmpfs) != 1 || h.Tmpfs["/tmp"] != "" || len(h.Mounts) != 3 {
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

func (m *Manager) namedCaddyID(ctx context.Context, name string) (string, error) {
	out, err := m.command(ctx, "docker", "container", "ls", "--all", "--no-trunc", "--filter", "name=^"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id != "" && !imageIdentity.MatchString("sha256:"+id) {
		return "", errors.New("invalid Caddy container inventory")
	}
	return id, nil
}

// findCaddy never adopts a named container from a different installation.
func (m *Manager) findCaddy(ctx context.Context, s *State) (*caddyContainer, error) {
	id, err := m.namedCaddyID(ctx, CaddyContainer)
	if err != nil || id == "" {
		return nil, err
	}
	data, err := m.command(ctx, "docker", "container", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return nil, err
	}
	var c caddyContainer
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("unrecognized Caddy container inspection: %w", err)
	}
	if c.ID != id {
		return nil, errors.New("Caddy container identity changed during inspection")
	}
	if s.ID == "" || c.Config.Labels[caddyHostLabel] != s.ID {
		return nil, fmt.Errorf("container %s is not owned by this installation; refusing to adopt or replace it", CaddyContainer)
	}
	tagIDs, err := m.caddyImageIDs(ctx, CaddyImage)
	if err != nil {
		return nil, err
	}
	c.ownsTag = strings.HasPrefix(ownedCaddyDigest(c.Config.Image), caddyRepo+"@") && slices.Equal(tagIDs, []string{c.Image})
	if c.ownsTag {
		ref, id, err := m.inspectCaddyImage(ctx)
		if err != nil {
			return nil, err
		}
		if id != c.Image {
			return nil, errors.New("Caddy image tag moved during inspection")
		}
		c.tagDigest = digestRef(ref)
	}
	return &c, nil
}

// ensureCaddy converges the ingress container: create it when absent, replace
// it when its image or shape differs from this release, and start it when
// stopped. Routes and certificates live on the host, so replacement keeps them.
func (m *Manager) ensureCaddy(ctx context.Context, s *State) error {
	return m.convergeCaddy(ctx, s, false)
}

var imageIdentity = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Resolve the pulled tag before interrupting ingress. The manifest digest is
// used for run and cleanup; Docker's platform image ID is used for comparison.
func (m *Manager) selectCaddy(ctx context.Context) (string, string, error) {
	if _, err := m.command(ctx, "docker", "pull", "--quiet", CaddyImage); err != nil {
		return "", "", err
	}
	return m.inspectCaddyImage(ctx)
}

func (m *Manager) inspectCaddyImage(ctx context.Context) (string, string, error) {
	data, err := m.command(ctx, "docker", "image", "inspect", "--format", "{{json .}}", CaddyImage)
	if err != nil {
		return "", "", err
	}
	var image struct {
		ID          string
		RepoDigests []string
		Descriptor  struct{ Digest string }
	}
	if err = json.Unmarshal(data, &image); err != nil {
		return "", "", err
	}
	if !imageIdentity.MatchString(image.ID) {
		return "", "", errors.New("invalid platform ID for Caddy image")
	}
	// The containerd image store exposes the exact descriptor for the tag. On
	// classic stores accept only an unambiguous repository digest.
	selected := ""
	for _, d := range image.RepoDigests {
		digest, ok := strings.CutPrefix(d, caddyRepo+"@")
		if !ok || !imageIdentity.MatchString(digest) {
			continue
		}
		if image.Descriptor.Digest != "" && digest != image.Descriptor.Digest {
			continue
		}
		if selected != "" && selected != digest {
			return "", "", errors.New("ambiguous manifest digest for Caddy image")
		}
		selected = digest
	}
	if selected == "" {
		return "", "", errors.New("missing manifest digest for Caddy image")
	}
	return CaddyImage + "@" + selected, image.ID, nil
}

func (m *Manager) refreshCaddy(ctx context.Context, s *State) error {
	return m.convergeCaddy(ctx, s, true)
}

func (m *Manager) convergeCaddy(ctx context.Context, s *State, daily bool) (err error) {
	// A hard shutdown may have interrupted the swap before rollback ran.
	// Don't treat that as a fresh installation or discard a serving container.
	previous, err := m.namedCaddyID(ctx, CaddyContainer+"-previous")
	if err != nil {
		return err
	}
	if previous != "" {
		return fmt.Errorf("vamos-caddy-previous (%s) already exists; compare both ingress containers with the host receipt (%s) before retrying; see Caddy ingress recovery in docs/host-lifecycle.md", previous, s.CaddyContainerID)
	}
	c, err := m.findCaddy(ctx, s)
	if err != nil {
		return err
	}
	if daily {
		if c == nil || c.ID != s.CaddyContainerID || !c.current() || !c.State.Running {
			return errors.New("managed Caddy container is missing or changed; run vamos server upgrade")
		}
		path := m.path(caddyfilePath)
		if err = rejectSymlink(path); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("unsafe managed Caddyfile")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(data), caddyConfig) {
			return errors.New("managed Caddyfile differs from this release")
		}
		if err = m.checkCaddyRuntime(ctx); err != nil {
			return err
		}
	}
	ref, id, err := m.selectCaddy(ctx)
	if err != nil {
		return err
	}
	if c != nil && c.current() && c.Image == id {
		if !c.State.Running {
			if _, err = m.command(ctx, "docker", "container", "start", c.ID); err != nil {
				return err
			}
		}
	} else {
		for _, dir := range []string{caddyDataDir, caddyConfigDir} {
			if err = rejectSymlink(m.path(dir)); err != nil {
				return err
			}
			if err = os.MkdirAll(m.path(dir), 0700); err != nil {
				return err
			}
		}
		var createdID string
		if c != nil {
			old := c
			if _, err = m.command(ctx, "docker", "container", "rename", old.ID, CaddyContainer+"-previous"); err != nil {
				return err
			}
			defer func() { err = m.finishCaddyReplacement(ctx, s, old, ref, id, createdID, err) }()
			if _, err = m.command(ctx, "docker", "container", "stop", old.ID); err != nil {
				return err
			}
		}
		output, runErr := m.command(ctx, "docker", caddyRunArgs(s.ID, ref)...)
		if runErr != nil {
			return runErr
		}
		createdID = strings.TrimSpace(string(output))
		if !imageIdentity.MatchString("sha256:" + createdID) {
			createdID = ""
			return errors.New("invalid Docker run container ID")
		}
		c, err = m.findCaddy(ctx, s)
		if err != nil {
			return err
		}
		if c == nil || c.ID != createdID || c.Image != id || c.Config.Image != ref || !c.current() || !c.State.Running {
			return errors.New("unexpected Caddy container after replacement")
		}
		if daily {
			if err = m.waitCaddyRuntime(ctx); err != nil {
				return err
			}
		}
	}
	if s.CaddyContainerID != c.ID {
		next := *s
		next.CaddyContainerID = c.ID
		if err = m.save(&next); err != nil {
			return err
		}
		s.CaddyContainerID = c.ID
	}
	return nil
}

// Docker detach does not wait for the new admin listener to be ready.
func (m *Manager) waitCaddyRuntime(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		err := m.checkCaddyRuntime(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Keep the previous container by ID until verification and receipt persistence
// both succeed. Recovery must also work after cancellation of the update.
func (m *Manager) finishCaddyReplacement(ctx context.Context, s *State, old *caddyContainer, ref, id, createdID string, err error) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	cmd := func(args ...string) error { _, e := m.command(recovery, "docker", args...); return e }
	if err == nil {
		err = cmd("container", "rm", "--force", old.ID)
		if err == nil {
			for _, previous := range slices.Compact([]string{ownedCaddyDigest(old.Config.Image), old.tagDigest}) {
				if previous != "" && previous != digestRef(ref) {
					_, _ = m.command(recovery, "docker", "image", "rm", previous)
				}
			}
		}
		return err
	}
	var e error
	if createdID != "" {
		e = cmd("container", "rm", "--force", createdID)
	} else {
		var replacement *caddyContainer
		replacement, e = m.findCaddy(recovery, s)
		if e == nil && replacement != nil {
			if replacement.Config.Image != ref {
				e = errors.New("unexpected replacement image")
			} else {
				e = cmd("container", "rm", "--force", replacement.ID)
			}
		}
	}

	if e == nil {
		e = cmd("container", "rename", old.ID, CaddyContainer)
	}
	if old.State.Running {
		e = errors.Join(e, cmd("container", "start", old.ID))
	}
	// Undo the pull's tag move only while it still names our candidate. An
	// unrelated retag must never be deleted to make the inventory look clean.
	if e == nil && old.Image != id && strings.HasPrefix(ref, CaddyImage+"@") {
		tags, inspectErr := m.caddyImageIDs(recovery, CaddyImage)
		e = inspectErr
		if e == nil && slices.Equal(tags, []string{id}) {
			if old.ownsTag {
				e = cmd("image", "tag", old.tagDigest, CaddyImage)
			} else {
				e = cmd("image", "rm", CaddyImage)
			}
			if e == nil {
				digest := digestRef(ref)
				ids, inspectErr := m.caddyImageIDs(recovery, digest)
				e = inspectErr
				if e == nil && slices.Equal(ids, []string{id}) {
					e = cmd("image", "rm", digest)
				}
			}
		}
	}
	return errors.Join(err, e)
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
