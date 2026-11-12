package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

var resourceID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var resourceFile = regexp.MustCompile(`^([0-9a-f]{32})\.(context\.tar|image\.iid)$`)

type resourceInspection struct {
	inspection
	Name                  string
	Image                 string
	RepoTags, RepoDigests []string
}
type resourceRow struct{ ID, Name, Names string }

func (m *Manager) resourceInspect(ctx context.Context, kind, id string) (*resourceInspection, error) {
	b, e := m.cmd(ctx, "docker", kind, "inspect", id)
	if e != nil {
		return nil, e
	}
	var rows []resourceInspection
	if e = json.Unmarshal(b, &rows); e != nil {
		return nil, e
	}
	if len(rows) != 1 {
		return nil, errors.New("invalid inspection count")
	}
	return &rows[0], nil
}
func (m *Manager) resourceList(ctx context.Context, kind string) ([]resourceRow, error) {
	args := []string{kind, "ls", "--no-trunc", "--format", "{{json .}}"}
	if kind == "container" {
		args = append(args, "--all")
	}
	b, e := m.cmd(ctx, "docker", args...)
	if e != nil {
		return nil, e
	}
	rows := []resourceRow{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r resourceRow
		if e = json.Unmarshal([]byte(line), &r); e != nil {
			return nil, e
		}
		if r.Name == "" {
			r.Name = r.Names
		}
		if !resourceID.MatchString(r.ID) || r.Name == "" || seen[r.ID] {
			return nil, errors.New("invalid Docker inventory")
		}
		seen[r.ID] = true
		rows = append(rows, r)
	}
	return rows, nil
}
func resourceContainer(v *resourceInspection, j *journal, r resourceRow) error {
	op := v.Config.Labels["soar.operation"]
	if !operation.MatchString(op) || !owned(v.Config.Labels, j, op) || r.Name != "soar-"+j.App+"-"+op || v.Name != "/"+r.Name || v.ID != r.ID || !imageID.MatchString(v.Image) || len(v.Mounts) != 0 || len(v.Config.Volumes) != 0 || len(v.HostConfig.Binds) != 0 || v.HostConfig.Privileged || v.HostConfig.NetworkMode != network(j) || len(v.NetworkSettings.Networks) != 1 || v.NetworkSettings.Networks[network(j)] == nil {
		return errors.New("unsafe or changed maintenance container")
	}
	return nil
}
func resourceRead(path string, limit int64) ([]byte, string, error) {
	s, e := os.Lstat(path)
	if e != nil {
		return nil, "", e
	}
	if !s.Mode().IsRegular() || s.Size() > limit {
		return nil, "", errors.New("unsafe maintenance file")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, "", e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, "", e
	}
	if !os.SameFile(s, st) {
		return nil, "", errors.New("maintenance file changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, "", e
	}
	if int64(len(b)) > limit {
		return nil, "", errors.New("maintenance file too large")
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}
func (m *Manager) resourceImages(ctx context.Context) (map[string]*resourceInspection, error) {
	b, e := m.cmd(ctx, "docker", "image", "ls", "--all", "--no-trunc", "--quiet")
	if e != nil {
		return nil, e
	}
	out := map[string]*resourceInspection{}
	for _, id := range strings.Fields(string(b)) {
		if !imageID.MatchString(id) {
			return nil, errors.New("invalid image inventory")
		}
		if out[id] != nil {
			continue
		}
		v, e := m.resourceInspect(ctx, "image", id)
		if e != nil {
			return nil, e
		}
		if v.ID != id {
			return nil, errors.New("changed image identity")
		}
		out[id] = v
	}
	return out, nil
}

// References include stopped and foreign containers and every other app's pending intent.
func (m *Manager) resourceReferences(ctx context.Context, j *journal, exclude map[string]bool) (map[string]bool, error) {
	refs := map[string]bool{}
	rows, e := m.resourceList(ctx, "container")
	if e != nil {
		return nil, e
	}
	for _, r := range rows {
		v, e := m.resourceInspect(ctx, "container", r.ID)
		if e != nil {
			return nil, e
		}
		if v.ID != r.ID || !imageID.MatchString(v.Image) {
			return nil, errors.New("invalid container image reference")
		}
		if !exclude[r.ID] {
			refs[v.Image] = true
			// Docker's containerd store may expose a config digest as Image and
			// the requested OCI manifest/index digest separately as Config.Image.
			if imageID.MatchString(v.Config.Image) {
				refs[v.Config.Image] = true
			}
		}
	}
	dirs, e := os.ReadDir(filepath.Dir(m.dir(j.App)))
	if e != nil {
		return nil, e
	}
	for _, d := range dirs {
		if d.Name() == j.App {
			continue
		}
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("unknown app state entry")
		}
		other, e := m.load(d.Name())
		if e != nil {
			return nil, e
		}
		for _, r := range []*release{other.Current, other.Previous, other.Candidate} {
			if r != nil {
				refs[r.Image] = true
			}
		}
		// Decode the optional intent independently so this helper does not depend on orchestration fields.
		b, _, e := resourceRead(filepath.Join(m.dir(d.Name()), "state.json"), 2<<20)
		if e != nil {
			return nil, e
		}
		var pending struct{ Maintenance *maintenance }
		if e = json.Unmarshal(b, &pending); e != nil {
			return nil, e
		}
		if pending.Maintenance != nil {
			for _, id := range pending.Maintenance.Images {
				refs[id] = true
			}
		}
	}
	return refs, nil
}
func (m *Manager) planMaintenance(ctx context.Context, j *journal, action, op string) (*maintenance, error) {
	if (action != "cleanup" && action != "destroy") || !operation.MatchString(op) {
		return nil, errors.New("invalid maintenance request")
	}
	w := &maintenance{Action: action, Operation: op, Plan: Plan{Action: action, App: j.App}}
	keep := map[string]bool{j.Operation: true}
	keepImages := map[string]bool{}
	for _, r := range []*release{j.Current, j.Previous, j.Candidate} {
		if r != nil {
			keep[r.Operation] = true
			keepImages[r.Image] = true
		}
	}
	rows, e := m.resourceList(ctx, "container")
	if e != nil {
		return nil, e
	}
	exclude := map[string]bool{}
	for _, r := range rows {
		v, e := m.resourceInspect(ctx, "container", r.ID)
		if e != nil {
			return nil, e
		}
		if !owned(v.Config.Labels, j, "") {
			prefix := "soar-" + j.App + "-"
			if strings.HasPrefix(r.Name, prefix) && operation.MatchString(strings.TrimPrefix(r.Name, prefix)) {
				return nil, errors.New("foreign app container name")
			}
			continue
		}
		if e = resourceContainer(v, j, r); e != nil {
			return nil, e
		}
		o := v.Config.Labels["soar.operation"]
		if action == "cleanup" && (keep[o] || v.State.Running) {
			keep[o] = true
			keepImages[v.Image] = true
			w.Plan.Retained = append(w.Plan.Retained, r.Name+": active or retained release")
			continue
		}
		w.Containers = append(w.Containers, plannedContainer{Name: r.Name, ID: r.ID, Image: v.Image, Operation: o})
		w.Plan.Containers = append(w.Plan.Containers, r.Name)
		exclude[r.ID] = true
	}
	nets, e := m.resourceList(ctx, "network")
	if e != nil {
		return nil, e
	}
	found := false
	for _, r := range nets {
		if r.Name != network(j) {
			continue
		}
		found = true
		v, e := m.resourceInspect(ctx, "network", r.ID)
		if e != nil {
			return nil, e
		}
		if v.ID != r.ID || v.Name != r.Name || !owned(v.Labels, j, "") || v.Driver != "bridge" || v.Internal {
			return nil, errors.New("unsafe app network")
		}
		for i, c := range w.Containers {
			cv, e := m.resourceInspect(ctx, "container", c.ID)
			if e != nil {
				return nil, e
			}
			var attachment struct{ NetworkID string }
			if e = json.Unmarshal(cv.NetworkSettings.Networks[r.Name], &attachment); e != nil {
				return nil, e
			}
			if attachment.NetworkID != r.ID {
				return nil, errors.New("container network identity mismatch")
			}
			w.Containers[i].NetworkID = r.ID
		}
		for id := range v.Containers {
			if !exclude[id] {
				if action == "destroy" {
					return nil, errors.New("unplanned network attachment")
				}
				known := false
				for _, c := range rows {
					if c.ID == id {
						cv, e := m.resourceInspect(ctx, "container", id)
						if e != nil {
							return nil, e
						}
						known = owned(cv.Config.Labels, j, "")
					}
				}
				if !known {
					return nil, errors.New("foreign network attachment")
				}
			}
		}
		if action == "destroy" {
			w.Network = &plannedNetwork{Name: r.Name, ID: r.ID}
			w.Plan.Network = r.Name
		}
	}
	if !found && len(w.Containers) > 0 {
		return nil, errors.New("owned network missing")
	}
	entries, e := os.ReadDir(m.dir(j.App))
	if e != nil {
		return nil, e
	}
	proof := map[string]bool{}
	for _, entry := range entries {
		n := entry.Name()
		if n == "state.json" {
			continue
		}
		match := resourceFile.FindStringSubmatch(n)
		if match == nil {
			return nil, fmt.Errorf("unknown app file %s", n)
		}
		limit := int64(32 << 20)
		if match[2] == "image.iid" {
			limit = 128
		}
		b, h, e := resourceRead(filepath.Join(m.dir(j.App), n), limit)
		if e != nil {
			return nil, e
		}
		if match[2] == "image.iid" {
			id := strings.TrimSpace(string(b))
			if !imageID.MatchString(id) {
				return nil, errors.New("invalid IID provenance")
			}
			proof[id] = true
			if keep[match[1]] {
				keepImages[id] = true
			}
			if action == "cleanup" {
				continue
			}
		} else if action == "cleanup" && keep[match[1]] {
			continue
		}
		w.Files = append(w.Files, plannedFile{Name: n, SHA256: h})
		w.Plan.Archives = append(w.Plan.Archives, n)
	}
	images, e := m.resourceImages(ctx)
	if e != nil {
		return nil, e
	}
	refs, e := m.resourceReferences(ctx, j, exclude)
	if e != nil {
		return nil, e
	}
	for id := range proof {
		v := images[id]
		if v == nil {
			continue
		}
		reason := ""
		if len(v.RepoTags) > 0 || len(v.RepoDigests) > 0 {
			reason = "tagged or repository digest"
		} else if refs[id] {
			reason = "referenced"
		} else if action == "cleanup" && keepImages[id] {
			reason = "retained release"
		}
		if reason != "" {
			if action == "destroy" {
				reason += "; provenance removed, manual cleanup required"
			}
			w.Plan.Retained = append(w.Plan.Retained, id+": "+reason)
			continue
		}
		w.Images = append(w.Images, id)
		w.Plan.Images = append(w.Plan.Images, id)
	}
	if len(w.Containers)+len(w.Images)+len(w.Files)+len(w.Plan.Retained) > 256 {
		return nil, errors.New("maintenance plan too large")
	}
	sort.Strings(w.Images)
	sort.Strings(w.Plan.Images)
	sort.Strings(w.Plan.Retained)
	b, e := json.Marshal(w.Plan)
	if e != nil {
		return nil, e
	}
	if len(b) > 8192 {
		return nil, errors.New("maintenance output too large")
	}
	return w, nil
}

func (m *Manager) plannedResourceContainer(ctx context.Context, j *journal, p plannedContainer) (*resourceInspection, error) {
	if !resourceID.MatchString(p.ID) || !resourceID.MatchString(p.NetworkID) || !imageID.MatchString(p.Image) || !operation.MatchString(p.Operation) || p.Name != "soar-"+j.App+"-"+p.Operation {
		return nil, errors.New("invalid planned container")
	}
	rows, e := m.resourceList(ctx, "container")
	if e != nil {
		return nil, e
	}
	present := false
	for _, r := range rows {
		if r.Name == p.Name && r.ID != p.ID {
			return nil, errors.New("same-name replacement container")
		}
		if r.ID == p.ID {
			if r.Name != p.Name {
				return nil, errors.New("renamed container")
			}
			present = true
		}
	}
	if !present {
		return nil, nil
	}
	v, e := m.resourceInspect(ctx, "container", p.ID)
	if e != nil {
		return nil, e
	}
	if e = resourceContainer(v, j, resourceRow{ID: p.ID, Name: p.Name}); e != nil {
		return nil, e
	}
	if v.Image != p.Image || v.Config.Labels["soar.operation"] != p.Operation {
		return nil, errors.New("changed planned container")
	}
	nv, e := m.resourceInspect(ctx, "network", network(j))
	if e != nil {
		return nil, e
	}
	if nv.ID != p.NetworkID || nv.Name != network(j) || !owned(nv.Labels, j, "") || nv.Driver != "bridge" || nv.Internal {
		return nil, errors.New("unsafe container network")
	}
	var attachment struct{ NetworkID string }
	if e = json.Unmarshal(v.NetworkSettings.Networks[network(j)], &attachment); e != nil {
		return nil, e
	}
	if attachment.NetworkID != nv.ID {
		return nil, errors.New("container network identity changed")
	}
	return v, nil
}
func syncResourceDir(path string) error {
	// Deployment hosts are Linux; Windows clients run filesystem-backed tests.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (m *Manager) removeMaintenanceResources(ctx context.Context, j *journal, w *maintenance) error {
	if w.Action != "cleanup" && w.Action != "destroy" {
		return errors.New("invalid maintenance action")
	}
	for _, p := range w.Containers {
		if w.Network != nil && p.NetworkID != w.Network.ID {
			return errors.New("planned container network identity mismatch")
		}
		commands := [][]string{{"container", "rm", p.ID}}
		if w.Action == "destroy" {
			commands = [][]string{{"container", "update", "--restart=no", p.ID}, {"container", "stop", p.ID}, {"container", "rm", p.ID}}
		}
		for _, args := range commands {
			v, e := m.plannedResourceContainer(ctx, j, p)
			if e != nil {
				return e
			}
			if v == nil {
				break
			}
			if w.Action == "cleanup" && v.State.Running {
				return errors.New("cleanup container started since planning")
			}
			if _, e = m.cmd(ctx, "docker", args...); e != nil {
				return e
			}
		}
	}
	for _, id := range w.Images {
		if !imageID.MatchString(id) {
			return errors.New("invalid planned image")
		}
		images, e := m.resourceImages(ctx)
		if e != nil {
			return e
		}
		v := images[id]
		if v == nil {
			continue
		}
		refs, e := m.resourceReferences(ctx, j, nil)
		if e != nil {
			return e
		}
		if len(v.RepoTags) > 0 || len(v.RepoDigests) > 0 || refs[id] { // Retention is safer than deleting a newly shared resource. Do not rewrite durable intent.
			continue
		}
		// The original IID must still be present before removing its image.
		entries, e := os.ReadDir(m.dir(j.App))
		if e != nil {
			return e
		}
		proven := false
		for _, entry := range entries {
			match := resourceFile.FindStringSubmatch(entry.Name())
			if match == nil || match[2] != "image.iid" {
				continue
			}
			b, _, e := resourceRead(filepath.Join(m.dir(j.App), entry.Name()), 128)
			if e != nil {
				return e
			}
			if strings.TrimSpace(string(b)) == id {
				proven = true
			}
		}
		if !proven {
			return errors.New("image provenance missing")
		}
		if _, e = m.cmd(ctx, "docker", "image", "rm", id); e != nil {
			return e
		}
	}
	if w.Network != nil {
		p := w.Network
		if w.Action != "destroy" || p.Name != network(j) || !resourceID.MatchString(p.ID) {
			return errors.New("invalid planned network")
		}
		rows, e := m.resourceList(ctx, "network")
		if e != nil {
			return e
		}
		present := false
		for _, r := range rows {
			if r.Name == p.Name && r.ID != p.ID {
				return errors.New("replacement network")
			}
			if r.ID == p.ID {
				if r.Name != p.Name {
					return errors.New("renamed network")
				}
				present = true
			}
		}
		if present {
			v, e := m.resourceInspect(ctx, "network", p.ID)
			if e != nil {
				return e
			}
			if v.ID != p.ID || v.Name != p.Name || !owned(v.Labels, j, "") || v.Driver != "bridge" || v.Internal || len(v.Containers) != 0 {
				return errors.New("unsafe or occupied planned network")
			}
			if _, e = m.cmd(ctx, "docker", "network", "rm", p.ID); e != nil {
				return e
			}
		}
	}
	for _, p := range w.Files {
		match := resourceFile.FindStringSubmatch(p.Name)
		if match == nil || !resourceID.MatchString(p.SHA256) {
			return errors.New("invalid planned file")
		}
		if w.Action == "cleanup" && match[2] == "image.iid" {
			return errors.New("cleanup cannot remove provenance")
		}
		limit := int64(32 << 20)
		if match[2] == "image.iid" {
			limit = 128
		}
		path := filepath.Join(m.dir(j.App), p.Name)
		_, h, e := resourceRead(path, limit)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil {
			if h != p.SHA256 {
				return errors.New("maintenance file changed")
			}
			if e = os.Remove(path); e != nil {
				return e
			}
		}
		if e = syncResourceDir(m.dir(j.App)); e != nil {
			return e
		}
	}
	return nil
}
