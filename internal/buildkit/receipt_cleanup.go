package buildkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// OwnedResources describes the Docker and containerd resources recorded in
// this host's verified BuildKit receipt.
type OwnedResources struct {
	ContainerID string
	Network     string
	Volume      string
	ImageID     string
	ImageRef    string
}

// OwnsContainerd reports whether a containerd resource in Docker's moby
// namespace belongs to this host's builder container or pinned image.
func (o *OwnedResources) OwnsContainerd(resource, item string) bool {
	if o == nil {
		return false
	}
	switch resource {
	case "containers":
		return o.ContainerID != "" && item == o.ContainerID
	case "images":
		name, digest, ok := strings.Cut(item, "@")
		if !ok || (name != "docker.io/moby/buildkit" && !strings.HasPrefix(name, "docker.io/moby/buildkit:")) {
			return false
		}
		return o.ImageRef != "" && strings.HasSuffix(o.ImageRef, "@"+digest)
	}
	return false
}

// InspectReceipt validates the recorded build-service receipt on disk and
// returns its owned resource identities without requiring them to be removed.
func (m Manager) InspectReceipt(hostID string) (*OwnedResources, error) {
	b, err := m.existingReceipt(hostID, false)
	if err != nil || b == nil {
		return nil, err
	}
	ref := ""
	if parts := strings.Split(b.s.Image, "@"); len(parts) == 2 {
		ref = "moby/buildkit@" + parts[1]
	}
	return &OwnedResources{
		ContainerID: b.s.ContainerID,
		Network:     b.s.Network,
		Volume:      b.s.Volume,
		ImageID:     b.s.ImageID,
		ImageRef:    ref,
	}, nil
}

// RemoveResources deletes the recorded builder container, network, cache
// volume and pinned image during an authorized host teardown.
func (m Manager) RemoveResources(ctx context.Context, hostID string) error {
	b, err := m.existingReceipt(hostID, true)
	if err != nil || b == nil {
		return err
	}
	if id, err := b.inventory(ctx, "container", b.s.Container); err != nil {
		return err
	} else if id != "" {
		if b.s.ContainerID == "" || id != b.s.ContainerID {
			return errInfra
		}
		if _, err := b.docker(ctx, "container", "rm", "--force", id); err != nil {
			return errInfra
		}
	}
	if id, err := b.inventory(ctx, "network", b.s.Network); err != nil {
		return err
	} else if id != "" {
		if b.s.NetworkID == "" || id != b.s.NetworkID {
			return errInfra
		}
		if _, err := b.docker(ctx, "network", "rm", id); err != nil {
			return errInfra
		}
	}
	if id, err := b.inventory(ctx, "volume", b.s.Volume); err != nil {
		return err
	} else if id != "" {
		var v volumeInfo
		if b.s.VolumeCreated == "" || b.inspect(ctx, "volume", id, &v) != nil {
			return errInfra
		}
		if v.Name != b.s.Volume || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 || !reflect.DeepEqual(v.Labels, b.s.labels("volume")) || !sameInstant(v.CreatedAt, b.s.VolumeCreated) || v.Mountpoint != b.s.VolumeMountpoint {
			return errInfra
		}
		if _, err := b.docker(ctx, "volume", "rm", b.s.Volume); err != nil {
			return errInfra
		}
	}
	if b.s.ImageID != "" {
		ref := "moby/buildkit@" + strings.Split(b.s.Image, "@")[1]
		r, err := b.docker(ctx, "image", "ls", "--quiet", "--no-trunc", ref)
		if err != nil {
			return errInfra
		}
		ids := strings.Fields(string(r.Stdout))
		for _, id := range ids {
			if id != b.s.ImageID {
				return errInfra
			}
		}
		if len(ids) > 0 {
			if _, err := b.docker(ctx, "image", "rm", ref); err != nil {
				return errInfra
			}
		}
	}
	return nil
}

// ReceiptGone proves that the recorded infrastructure was explicitly removed.
// It never removes Docker resources or cache. The caller holds the host lock.
func (m Manager) ReceiptGone(ctx context.Context, hostID string) error {
	b, err := m.existingReceipt(hostID, false)
	if err != nil || b == nil {
		return err
	}
	return b.resourcesGone(ctx)
}

func (m Manager) existingReceipt(hostID string, allowEmpty bool) (*build, error) {
	if !hostPattern.MatchString(hostID) {
		return nil, errInfra
	}
	root := m.Root
	if root == "" {
		root = "/"
	}
	dir := filepath.Join(root, "var/lib/vamos/buildkit")
	st, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !st.IsDir() || !ownedFile(st) || st.Mode().Perm()&0077 != 0 || safeDir(dir, false) != nil {
		return nil, errInfra
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errInfra
	}
	// Only an already-authorized host purge may reconcile the final unlink.
	if len(entries) == 0 && allowEmpty {
		return nil, os.Remove(dir)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".receipt-") {
			continue
		}
		if entry.Name() != "state.json" && entry.Name() != "buildx" {
			return nil, errInfra
		}
	}
	s, dir, err := m.state(hostID)
	if err != nil || metadataTree(dir) != nil {
		return nil, errInfra
	}
	return &build{m: m, s: s, dir: dir}, nil
}

func (b *build) resourcesGone(ctx context.Context) error {
	for kind, name := range map[string]string{"container": b.s.Container, "network": b.s.Network, "volume": b.s.Volume} {
		id, err := b.inventory(ctx, kind, name)
		if err != nil {
			return errInfra
		}
		if id != "" {
			return errors.New("retained build service resources block teardown; explicitly remove the recorded builder container, network and cache first")
		}
	}
	return nil
}

// RemoveReceipt removes only our exact guard rules and private disposable client
// metadata after resources are gone. The caller MUST hold the host lock, have
// persisted host-purge authorization, and have verified Docker is otherwise empty.
func (m Manager) RemoveReceipt(ctx context.Context, hostID string) error {
	b, err := m.existingReceipt(hostID, true)
	if err != nil || b == nil {
		return err
	}
	if err := b.resourcesGone(ctx); err != nil {
		return err
	}
	for _, exe := range []string{"/usr/sbin/iptables", "/usr/sbin/ip6tables"} {
		for _, chain := range []string{"PREROUTING", "FORWARD"} {
			rules, err := b.rules(ctx, exe, chain)
			if err != nil {
				return err
			}
			for _, rule := range rules {
				owned := false
				for _, guard := range b.guards() {
					owned = owned || (guard.chain == chain && reflect.DeepEqual(rule, guard.args))
				}
				if !owned {
					if strings.Contains(strings.Join(rule, " "), "vamos-buildkit:"+b.s.Host) {
						return errInfra
					}
					continue
				}
				args := append([]string{"-w", "5", "-t", "mangle", "-D", chain}, rule...)
				if _, err := b.command(ctx, exe, args, nil); err != nil {
					return errInfra
				}
			}
		}
	}
	// Only private Buildx metadata is disposable; root's Docker configuration
	// and credentials live elsewhere and are never managed by this package.
	if err := os.RemoveAll(filepath.Join(b.dir, "buildx")); err != nil {
		return errInfra
	}
	if err := syncDir(b.dir); err != nil {
		return errInfra
	}
	if err := os.Remove(filepath.Join(b.dir, "state.json")); err != nil {
		return errInfra
	}
	if err := syncDir(b.dir); err != nil {
		return errInfra
	}
	if err := os.Remove(b.dir); err != nil {
		return errInfra
	}
	parent, err := os.Open(filepath.Dir(b.dir))
	if err != nil {
		return errInfra
	}
	defer parent.Close()
	return parent.Sync()
}
