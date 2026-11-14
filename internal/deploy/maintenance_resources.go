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

	"github.com/nchapman/soar/internal/config"
)

var resourceID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var resourceFile = regexp.MustCompile(`^([0-9a-f]{32})\.(run\.json|context\.tar|(?:[0-9a-f]{32}\.)?image\.iid)$`)

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
		return nil, errors.New("invalid Docker maintenance inspection response")
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
	if service, present := v.Config.Labels["soar.service"]; present && service == "" {
		return errors.New("empty maintenance service label")
	}
	if !operation.MatchString(op) || !owned(v.Config.Labels, j, op) || !maintenanceContainerIdentity(j, r.Name, v.Config.Labels["soar.service"], op, replicaLabel(v.Config.Labels)) || v.Name != "/"+r.Name || v.ID != r.ID || !imageID.MatchString(v.Image) || (!supportsVolumes(j) && (len(v.Mounts) != 0 || len(v.HostConfig.Mounts) != 0 || len(v.HostConfig.VolumesFrom) != 0)) || (!supportsVolumes(j) && len(v.Config.Volumes) != 0) || len(v.HostConfig.Binds) != 0 || v.HostConfig.Privileged || v.HostConfig.NetworkMode != network(j) || len(v.NetworkSettings.Networks) != 1 || v.NetworkSettings.Networks[network(j)] == nil {
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
		for _, r := range retainedReleases(other) {
			if r != nil {
				refs[r.Image] = true
			}
		}
		for _, id := range other.BuildImages {
			refs[id] = true
		}
		// load hydrates either legacy inline or digest-bound paged authority.
		if other.Maintenance != nil {
			for _, id := range other.Maintenance.Images {
				refs[id] = true
			}
		}
	}
	return refs, nil
}
func (m *Manager) planMaintenance(ctx context.Context, j *journal, action, op string) (*maintenance, error) {
	return m.planMaintenanceScope(ctx, j, action, op, "")
}

func (m *Manager) planMaintenanceScope(ctx context.Context, j *journal, action, op, service string) (*maintenance, error) {
	if (action != "cleanup" && action != "destroy") || !operation.MatchString(op) {
		return nil, errors.New("invalid maintenance request")
	}
	if err := m.preflightRuns(ctx, j); err != nil {
		return nil, err
	}
	if err := m.maintenanceRuns(ctx, j); err != nil {
		return nil, err
	}
	w := &maintenance{Action: action, Operation: op, Plan: Plan{Action: action, App: j.App}}
	if err := m.maintenanceVolumes(ctx, j, &w.Plan); err != nil {
		return nil, err
	}
	keep := map[string]bool{j.Operation: true}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot != nil {
			keep[snapshot.Operation] = true
		}
	}
	keepImages := map[string]bool{}
	for _, r := range retainedReleases(j) {
		if r != nil {
			keep[r.Operation] = true
			keepImages[r.Image] = true
		}
	}
	for _, id := range j.BuildImages {
		keepImages[id] = true
	}
	rows, e := m.resourceList(ctx, "container")
	if e != nil {
		return nil, e
	}
	prune, e := m.maintenanceRunPrune(j)
	if e != nil {
		return nil, e
	}
	runBatch := 0
	exclude := map[string]bool{}
	for _, r := range rows {
		v, e := m.resourceInspect(ctx, "container", r.ID)
		if e != nil {
			return nil, e
		}
		if !owned(v.Config.Labels, j, "") {
			prefix := "soar-" + j.App + "-"
			if retainedContainer(j, r.Name) || (strings.HasPrefix(r.Name, prefix) && operation.MatchString(strings.TrimPrefix(r.Name, prefix))) {
				return nil, errors.New("foreign app container name")
			}
			continue
		}
		if e = m.verifyResourceContainer(ctx, v, j, r); e != nil {
			return nil, e
		}
		if service != "" && v.Config.Labels["soar.service"] != service {
			continue
		}
		o := v.Config.Labels["soar.operation"]
		if action == "cleanup" && (retainedContainer(j, r.Name) || v.State.Running) {
			keep[o] = true
			keepImages[v.Image] = true
			w.Plan.Retained = append(w.Plan.Retained, r.Name+": active or retained release")
			continue
		}
		// Select prunable runs first; never discard provenance for a container
		// deferred to a later cleanup. Once pruning is exhausted, drain retained runs.
		if action == "cleanup" && v.Config.Labels["soar.run"] != "" {
			record := v.Config.Labels["soar.run"] + ".run.json"
			if runBatch >= maintenanceRunPruneBatch || (len(prune) > 0 && !prune[record]) {
				delete(prune, record)
				continue
			}
			runBatch++
		}
		w.Containers = append(w.Containers, plannedContainer{Name: r.Name, ID: r.ID, Image: v.Image, Operation: o, Service: v.Config.Labels["soar.service"], Replica: replicaLabel(v.Config.Labels)})
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
				if action == "destroy" && service == "" {
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
	if service != "" {
		w.Action = "service-remove"
		w.Service = service
		w.Plan.Action = w.Action
		w.Network = nil
		w.Plan.Network = ""
		w.Plan.Retained = append(w.Plan.Retained, "service data, secrets, shared network, images and archives retained")
		sortMaintenanceContainers(w)
		return w, boundMaintenancePreview(&w.Plan)
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
		if strings.HasSuffix(match[2], "image.iid") {
			limit = 128
		}
		if match[2] == "run.json" {
			limit = runRecordLimit
		}
		if match[2] == "run.json" {
			if _, err := m.loadRun(j, match[1]); err != nil {
				return nil, err
			}
			if action == "cleanup" && !prune[n] {
				continue
			}
		}
		b, h, e := resourceRead(filepath.Join(m.dir(j.App), n), limit)
		if e != nil {
			return nil, e
		}
		if strings.HasSuffix(match[2], "image.iid") {
			id := strings.TrimSpace(string(b))
			if !imageID.MatchString(id) {
				// BuildKit may leave an empty IID on a failed, never-started run.
				// It grants no image authority, and is removed only with that run's
				// record. All unexplained/corrupt provenance still fails closed.
				if id != "" || !m.emptyUnstartedRunIID(ctx, j, match[1], n) {
					return nil, errors.New("invalid IID provenance")
				}
				if action == "cleanup" && !prune[match[1]+".run.json"] {
					continue
				}
			} else {
				proof[id] = true
				if keep[match[1]] {
					keepImages[id] = true
				}
				if action == "cleanup" {
					continue
				}
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
	sortMaintenanceContainers(w)
	sort.Strings(w.Images)
	sort.Strings(w.Plan.Images)
	sort.Strings(w.Plan.Retained)
	// Run records must be removed before their IID receipts. Files otherwise
	// retain lexicographic order; this order is captured, never recomputed on resume.
	sort.SliceStable(w.Files, func(i, k int) bool {
		a, b := strings.HasSuffix(w.Files[i].Name, ".run.json"), strings.HasSuffix(w.Files[k].Name, ".run.json")
		if a != b {
			return a
		}
		return w.Files[i].Name < w.Files[k].Name
	})
	w.Plan.Archives = nil
	for _, f := range w.Files {
		w.Plan.Archives = append(w.Plan.Archives, f.Name)
	}
	return w, boundMaintenancePreview(&w.Plan)
}

func (m *Manager) plannedResourceContainer(ctx context.Context, j *journal, p plannedContainer) (*resourceInspection, error) {
	if !resourceID.MatchString(p.ID) || !resourceID.MatchString(p.NetworkID) || !imageID.MatchString(p.Image) || !operation.MatchString(p.Operation) || !maintenanceContainerIdentity(j, p.Name, p.Service, p.Operation, p.Replica) {
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
	if e = m.verifyResourceContainer(ctx, v, j, resourceRow{ID: p.ID, Name: p.Name}); e != nil {
		return nil, e
	}
	if replicaLabel(v.Config.Labels) != p.Replica || v.Image != p.Image || v.Config.Labels["soar.operation"] != p.Operation || v.Config.Labels["soar.service"] != p.Service {
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
	if w.Action != "cleanup" && w.Action != "destroy" && w.Action != "service-remove" {
		return errors.New("invalid maintenance action")
	}
	if w.ResourcesDone {
		if err := m.verifyMaintenanceResourceBarrier(ctx, w); err != nil {
			return err
		}
		return m.removeMaintenanceFiles(j, w)
	}
	if err := m.maintenanceRuns(ctx, j); err != nil {
		return err
	}
	if err := m.maintenanceVolumes(ctx, j, nil); err != nil {
		return err
	}
	for _, p := range w.Containers {
		if w.Network != nil && p.NetworkID != w.Network.ID {
			return errors.New("planned container network identity mismatch")
		}
		commands := [][]string{{"container", "rm", p.ID}}
		if (w.Action == "destroy" || w.Action == "service-remove") && !strings.HasPrefix(p.Name, "soar-run-") {
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
			if supportsVolumes(j) && args[1] == "rm" && (v.State.Running || v.State.Restarting || v.State.Paused || ((w.Action == "destroy" || w.Action == "service-remove") && v.HostConfig.RestartPolicy.Name != "no")) {
				return errors.New("container writers are not proven stopped")
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
			if match == nil || !strings.HasSuffix(match[2], "image.iid") {
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
	if w.Manifest != nil {
		if err := m.verifyMaintenanceResourceBarrier(ctx, w); err != nil {
			return err
		}
		// Commit the dependency barrier before removing any provenance. Retained
		// shared images are not reconsidered after this barrier on replay.
		w.ResourcesDone = true
		if err := m.save(j); err != nil {
			w.ResourcesDone = false
			return err
		}
	}
	return m.removeMaintenanceFiles(j, w)
}

func (m *Manager) removeMaintenanceFiles(j *journal, w *maintenance) error {
	for _, p := range w.Files {
		match := resourceFile.FindStringSubmatch(p.Name)
		if match == nil || !resourceID.MatchString(p.SHA256) {
			return errors.New("invalid planned file")
		}
		if w.Action == "cleanup" && (strings.HasSuffix(match[2], "image.iid")) {
			return errors.New("cleanup cannot remove provenance")
		}
		limit := int64(32 << 20)
		if strings.HasSuffix(match[2], "image.iid") {
			limit = 128
		}
		if match[2] == "run.json" {
			limit = runRecordLimit
		}
		path := filepath.Join(m.dir(j.App), p.Name)
		_, h, e := resourceRead(path, limit)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil {
			if match[2] == "run.json" {
				if _, err := m.loadRun(j, match[1]); err != nil {
					return err
				}
			}
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

// Validate historical candidates by registry binding plus immutable image and
// container provenance, without recursively enumerating volume references.
func (m *Manager) verifyResourceContainer(ctx context.Context, v *resourceInspection, j *journal, row resourceRow) error {
	if v.Config.Labels["soar.run"] != "" || strings.HasPrefix(row.Name, "soar-run-") {
		return m.maintenanceRunContainer(ctx, j, v, row)
	}
	if err := resourceContainer(v, j, row); err != nil {
		return err
	}
	if err := m.replicaResourceProvenance(j, v, row.Name); err != nil {
		return err
	}
	if !supportsVolumes(j) {
		return nil
	}
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil {
		return err
	}
	service := v.Config.Labels["soar.service"]
	if s == nil || s.Records[service].ID == "" {
		if len(v.Mounts)+len(v.HostConfig.Mounts)+len(v.HostConfig.VolumesFrom)+len(v.Config.Volumes) != 0 {
			return errors.New("unregistered maintenance mounts")
		}
		return nil
	}
	rec := s.Records[service]
	if rec.State != "created" {
		return errors.New("maintenance volume is not created")
	}
	if _, yes, err := m.inspectVolume(ctx, j, rec); err != nil {
		return err
	} else if !yes {
		return errors.New("maintenance volume missing")
	}
	if err := validateVolumeMounts(&v.inspection, rec); err != nil {
		return err
	}
	image, err := m.resourceInspect(ctx, "image", v.Image)
	if err != nil {
		return err
	}
	cfg := &config.Service{Volume: &config.Volume{Mount: rec.Mount}}
	if image.ID != v.Image || imageVolumesAllowed(&image.inspection, cfg, true) != nil || imageVolumesAllowed(&v.inspection, cfg, true) != nil {
		return errors.New("maintenance volume container image provenance differs")
	}
	proven := false
	op := v.Config.Labels["soar.operation"]
	for _, rel := range retainedReleases(j) {
		if releaseHas(rel, row.Name) && runtimeLabelMatches(rel, row.Name, v.Config.Labels) && rel.Service == service && rel.Operation == op && rel.VolumeID == rec.ID && rel.Image == v.Config.Image {
			proven = true
		}
	}
	if !proven {
		entries, err := os.ReadDir(m.dir(j.App))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			match := resourceFile.FindStringSubmatch(entry.Name())
			if match == nil || match[1] != op || !strings.HasSuffix(match[2], "image.iid") {
				continue
			}
			b, _, err := resourceRead(filepath.Join(m.dir(j.App), entry.Name()), 128)
			if err != nil {
				return err
			}
			if imageID.MatchString(v.Config.Image) && strings.TrimSpace(string(b)) == v.Config.Image {
				proven = true
			}
		}
	}
	if !proven {
		return errors.New("maintenance container lacks captured image provenance")
	}
	return nil
}

func (m *Manager) maintenanceVolumes(ctx context.Context, j *journal, plan *Plan) error {
	if !supportsVolumes(j) {
		p := filepath.Join(m.volumeStore().Root, j.App+".json")
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil || s == nil {
		return err
	}
	names := make([]string, 0, len(s.Records))
	for name := range s.Records {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := s.Records[name]
		if _, yes, e := m.inspectVolume(ctx, j, rec); e != nil {
			return e
		} else if !yes && rec.State == "created" {
			return errors.New("retained volume missing")
		}
		if e := m.volumeReferences(ctx, j, rec, "", true); e != nil {
			return e
		}
		if plan != nil {
			plan.Retained = append(plan.Retained, "volume "+name+"/"+rec.Name+": retained; data is never deleted")
		}
	}
	if plan != nil {
		b, e := json.Marshal(plan)
		if e != nil || len(b) > maxMaintenancePlanBytes {
			return errors.New("maintenance metadata exceeds plan limit")
		}
	}
	return nil
}
