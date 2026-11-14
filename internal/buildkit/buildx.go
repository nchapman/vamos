package buildkit

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
)

// Buildx's store.NodeGroup metadata is deliberately checked before running a
// build. `buildx inspect` has no machine-readable format in Buildx v0.37.2 and
// can try to dial the daemon. Unknown store schema changes fail closed.
type nodeGroup struct {
	Name, Driver           string
	Dynamic, DockerContext bool
	Nodes                  []struct {
		Name, Endpoint string
		Platforms      []json.RawMessage
		DriverOpts     map[string]string
		Flags          []string
		Files          map[string][]byte
	}
}

func metadataTree(path string) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errInfra
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errInfra
		}
		info, err := d.Info()
		if err != nil || !ownedFile(info) || (!info.IsDir() && !info.Mode().IsRegular()) || info.Mode().Perm()&0022 != 0 {
			return errInfra
		}
		return nil
	})
}
func (b *build) builder(ctx context.Context) error {
	if safeDir(filepath.Join(b.dir, "buildx"), true) != nil || metadataTree(filepath.Join(b.dir, "buildx")) != nil {
		return errInfra
	}
	path := filepath.Join(b.dir, "buildx", "instances", b.s.Builder)
	raw, err := privateFile(path)
	if os.IsNotExist(err) {
		if _, err = b.docker(ctx, "buildx", "create", "--name", b.s.Builder, "--node", b.s.Builder+"0", "--driver", "remote", "docker-container://"+b.s.Container); err != nil {
			return errInfra
		}
		if metadataTree(filepath.Join(b.dir, "buildx")) != nil {
			return errInfra
		}
		raw, err = privateFile(path)
	}
	if err != nil {
		return errInfra
	}
	var group nodeGroup
	if strictJSON(raw, &group) != nil || group.Name != b.s.Builder || group.Driver != "remote" || group.Dynamic || group.DockerContext || len(group.Nodes) != 1 {
		return errInfra
	}
	n := group.Nodes[0]
	if n.Name != b.s.Builder+"0" || n.Endpoint != "docker-container://"+b.s.Container || len(n.Platforms) != 0 || len(n.DriverOpts) != 0 || len(n.Flags) != 0 || len(n.Files) != 0 {
		return errInfra
	}
	return nil
}
