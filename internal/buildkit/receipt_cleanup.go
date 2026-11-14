package buildkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

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
	dir := filepath.Join(root, "var/lib/soar/buildkit")
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
					if strings.Contains(strings.Join(rule, " "), "soar-buildkit:"+b.s.Host) {
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
