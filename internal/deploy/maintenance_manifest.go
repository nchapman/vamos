package deploy

// Private intent is a hash-linked sequence of bounded immutable pages. Only a
// completely published header may be installed in state.json. The header also
// binds the ready journal, so a publication whose journal write was interrupted
// can only be adopted by the same operation against the same deployment state.
// A preparing header grants no deletion authority. An orphan published header
// is never thrown away or replaced by a fresh inventory.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const maintenancePageBytes = 32 << 10
const maintenancePageItems = 32

type maintenanceManifestRef struct {
	Version int
	Root    string
	Binding string
	Pages   int
}

type maintenanceManifestHeader struct {
	Version            int
	HostID, App, Basis string
	Preparing          bool
	Intent             maintenance
}

type maintenanceManifestPage struct {
	Version                         int
	HostID, App, Operation, Binding string
	Index                           int
	Next                            string
	Containers                      []plannedContainer
	Images                          []string
	Files                           []plannedFile
}

func maintenanceDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (m *Manager) maintenanceManifestDir(app string) string {
	return m.path("/var/lib/soar/maintenance/" + app)
}
func maintenancePageName(i int) string { return fmt.Sprintf("%016x.json", i) }
func maintenanceDiskIntent(w *maintenance) maintenance {
	copy := *w
	copy.Containers = nil
	copy.Images = nil
	copy.Files = nil
	return copy
}
func maintenanceBasis(j *journal) (string, error) {
	b, e := json.Marshal(j)
	return maintenanceDigest(b), e
}
func maintenanceBinding(h maintenanceManifestHeader) (string, error) {
	h.Preparing = false
	h.Intent.Manifest = nil
	h.Intent.ResourcesDone = false
	b, e := json.Marshal(h)
	return maintenanceDigest(b), e
}
func readMaintenanceJSON(path string, limit int64, dst any) ([]byte, error) {
	if e := safe(path); e != nil {
		return nil, e
	}
	b, _, e := resourceRead(path, limit)
	if e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(dst); e != nil {
		return nil, errors.New("invalid maintenance manifest JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing maintenance manifest JSON")
	}
	return b, nil
}
func (m *Manager) maintenanceHeader(j *journal) (*maintenanceManifestHeader, error) {
	var h maintenanceManifestHeader
	_, e := readMaintenanceJSON(filepath.Join(m.maintenanceManifestDir(j.App), "intent.json"), 2<<20, &h)
	if e != nil {
		return nil, e
	}
	if h.Version != 1 || h.HostID != j.HostID || h.App != j.App || !fullResourceID.MatchString(h.Basis) || !operation.MatchString(h.Intent.Operation) || h.Intent.Plan.App != j.App || h.Intent.Plan.Action != h.Intent.Action || (h.Intent.Action != "cleanup" && h.Intent.Action != "destroy" && h.Intent.Action != "service-remove") || len(h.Intent.Containers)+len(h.Intent.Images)+len(h.Intent.Files) != 0 || h.Intent.ResourcesDone {
		return nil, errors.New("invalid maintenance manifest binding")
	}
	if h.Preparing != (h.Intent.Manifest == nil) {
		return nil, errors.New("invalid maintenance publication state")
	}
	return &h, nil
}

// Called before planning a new operation. A completed operation may leave its
// storage behind after a lost completion response, but no other orphan is GC'd.
func (m *Manager) preparedMaintenance(j *journal, action, op string) (*maintenance, error) {
	dir := m.maintenanceManifestDir(j.App)
	if e := safe(dir); e != nil {
		return nil, e
	}
	_, e := os.ReadDir(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	h, e := m.maintenanceHeader(j)
	if errors.Is(e, os.ErrNotExist) {
		return nil, m.discardUnpublishedMaintenance(j)
	}
	if e != nil {
		return nil, e
	}
	if maintenanceCompleted(j, h) {
		if e = m.discardMaintenanceStorage(j, h); e != nil {
			return nil, e
		}
		return nil, nil
	}
	basis, e := maintenanceBasis(j)
	if e != nil {
		return nil, e
	}
	if h.Intent.Operation != op || h.Intent.Action != action || h.Basis != basis {
		return nil, errors.New("prepared maintenance requires its exact operation and unchanged journal")
	}
	if h.Preparing {
		// No complete chain was ever published, and state.json still equals Basis.
		// Thus these files cannot have authorized any resource mutation.
		if e = m.discardMaintenanceStorage(j, h); e != nil {
			return nil, e
		}
		return nil, nil
	}
	w := h.Intent
	if e = m.hydrateMaintenance(j, &w); e != nil {
		return nil, e
	}
	return &w, nil
}

func (m *Manager) persistMaintenance(j *journal, w *maintenance) error {
	if w.Manifest != nil {
		return m.hydrateMaintenance(j, w)
	}
	dir := m.maintenanceManifestDir(j.App)
	if e := safe(dir); e != nil {
		return e
	}
	root := filepath.Dir(dir)
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	if e := syncResourceDir(filepath.Dir(root)); e != nil {
		return e
	}
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	if e := syncResourceDir(root); e != nil {
		return e
	}
	basis, e := maintenanceBasis(j)
	if e != nil {
		return e
	}
	h := maintenanceManifestHeader{Version: 1, HostID: j.HostID, App: j.App, Basis: basis, Preparing: true, Intent: maintenanceDiskIntent(w)}
	headerPath := filepath.Join(dir, "intent.json")
	b, e := json.Marshal(h)
	if e != nil {
		return e
	}
	if e = atomic(headerPath, b, 0600); e != nil {
		return e
	}
	binding, e := maintenanceBinding(h)
	if e != nil {
		return e
	}
	// Fixed-size typed batches bound both individual files and parser work. The
	// whole inventory is retained in memory for the existing cross-record proofs;
	// it is never serialized into the bounded deployment journal.
	pages := []maintenanceManifestPage{{}}
	add := func() *maintenanceManifestPage {
		p := &pages[len(pages)-1]
		if len(p.Containers)+len(p.Images)+len(p.Files) == maintenancePageItems {
			pages = append(pages, maintenanceManifestPage{})
			p = &pages[len(pages)-1]
		}
		return p
	}
	for _, v := range w.Containers {
		p := add()
		p.Containers = append(p.Containers, v)
	}
	for _, v := range w.Images {
		p := add()
		p.Images = append(p.Images, v)
	}
	for _, v := range w.Files {
		p := add()
		p.Files = append(p.Files, v)
	}
	next := ""
	for i := len(pages) - 1; i >= 0; i-- {
		p := &pages[i]
		p.Version = 1
		p.HostID = j.HostID
		p.App = j.App
		p.Operation = w.Operation
		p.Binding = binding
		p.Index = i
		p.Next = next
		b, e = json.Marshal(p)
		if e != nil {
			return e
		}
		if len(b) > maintenancePageBytes {
			return errors.New("maintenance page exceeds byte limit")
		}
		if e = atomic(filepath.Join(dir, maintenancePageName(i)), b, 0600); e != nil {
			return e
		}
		next = maintenanceDigest(b)
	}
	ref := &maintenanceManifestRef{Version: 1, Root: next, Binding: binding, Pages: len(pages)}
	h.Preparing = false
	h.Intent.Manifest = ref
	b, e = json.Marshal(h)
	if e != nil {
		return e
	}
	if e = atomic(headerPath, b, 0600); e != nil {
		return e
	}
	w.Manifest = ref
	return nil
}

func (m *Manager) hydrateMaintenance(j *journal, w *maintenance) error {
	ref := w.Manifest
	if ref == nil || ref.Version != 1 || ref.Pages < 1 || !fullResourceID.MatchString(ref.Root) || !fullResourceID.MatchString(ref.Binding) {
		return errors.New("invalid maintenance manifest reference")
	}
	h, e := m.maintenanceHeader(j)
	if e != nil {
		return e
	}
	disk := maintenanceDiskIntent(w)
	disk.ResourcesDone = false
	if h.Preparing || !reflect.DeepEqual(h.Intent, disk) {
		return errors.New("maintenance manifest differs from journal")
	}
	binding, e := maintenanceBinding(*h)
	if e != nil {
		return e
	}
	if binding != ref.Binding {
		return errors.New("maintenance manifest metadata digest mismatch")
	}
	entries, e := os.ReadDir(m.maintenanceManifestDir(j.App))
	if e != nil {
		return e
	}
	// Avoid an attacker-controlled count driving reads/allocations. No other file
	// (including a symlink or a leftover temporary write) is active authority.
	if len(entries) != ref.Pages+1 {
		return errors.New("maintenance manifest page set incomplete or unexpected")
	}
	var containers []plannedContainer
	var images []string
	var files []plannedFile
	digest := ref.Root
	for i := 0; i < ref.Pages; i++ {
		var p maintenanceManifestPage
		b, e := readMaintenanceJSON(filepath.Join(m.maintenanceManifestDir(j.App), maintenancePageName(i)), maintenancePageBytes, &p)
		if e != nil {
			return e
		}
		if maintenanceDigest(b) != digest || p.Version != 1 || p.HostID != j.HostID || p.App != j.App || p.Operation != w.Operation || p.Binding != binding || p.Index != i || len(p.Containers)+len(p.Images)+len(p.Files) > maintenancePageItems {
			return errors.New("maintenance manifest page digest or binding mismatch")
		}
		if (i == ref.Pages-1 && p.Next != "") || (i < ref.Pages-1 && !fullResourceID.MatchString(p.Next)) {
			return errors.New("invalid maintenance manifest chain")
		}
		containers = append(containers, p.Containers...)
		images = append(images, p.Images...)
		files = append(files, p.Files...)
		digest = p.Next
	}
	w.Containers = containers
	w.Images = images
	w.Files = files
	return nil
}

// GC requires either a never-published preparing header with an unchanged ready
// journal, or an exact durable completion receipt. Keep the header until every
// page unlink is synced, so interruption during GC remains classifiable. Only
// this dedicated namespace's regular page/temp files may be removed.
func (m *Manager) discardMaintenanceStorage(j *journal, h *maintenanceManifestHeader) error {
	dir := m.maintenanceManifestDir(j.App)
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		n := entry.Name()
		if n == "intent.json" {
			continue
		}
		valid := strings.HasPrefix(n, ".deploy-")
		if len(n) == 21 && strings.HasSuffix(n, ".json") {
			var i int
			if _, err := fmt.Sscanf(n, "%016x.json", &i); err == nil && i >= 0 && maintenancePageName(i) == n && (h.Preparing || i < h.Intent.Manifest.Pages) {
				valid = true
			}
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !valid || !info.Mode().IsRegular() {
			return errors.New("unknown maintenance storage file prevents recovery")
		}
	}
	for _, entry := range entries {
		if entry.Name() != "intent.json" {
			if e = os.Remove(filepath.Join(dir, entry.Name())); e != nil {
				return e
			}
		}
	}
	if e = syncResourceDir(dir); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(dir, "intent.json")); e != nil {
		return e
	}
	if e = syncResourceDir(dir); e != nil {
		return e
	}
	if e = os.Remove(dir); e != nil {
		return e
	}
	root := filepath.Dir(dir)
	if e = syncResourceDir(root); e != nil {
		return e
	}
	entries, e = os.ReadDir(root)
	if e != nil {
		return e
	}
	if len(entries) == 0 {
		if e = os.Remove(root); e != nil {
			return e
		}
		return syncResourceDir(filepath.Dir(root))
	}
	return nil
}

func (m *Manager) finishMaintenanceStorage(j *journal) error {
	_, e := m.preparedMaintenance(j, "", "")
	return e
}

// Validate the full prospective journal before publishing anything. The digest
// reference does not exist yet; this local-only placeholder selects the paged
// format's validation rules without imposing the legacy inline capacity limit.
// It is never saved or used for deletion. load always authenticates real pages
// before validating any journal with a persisted reference.
func validateMaintenanceCapture(j *journal) error {
	if j.Maintenance == nil || j.Maintenance.Manifest != nil {
		return validateJournal(j, j.App)
	}
	clone := *j
	w := *j.Maintenance
	w.Manifest = &maintenanceManifestRef{Version: 1, Root: strings.Repeat("0", 64), Binding: strings.Repeat("0", 64), Pages: 1}
	clone.Maintenance = &w
	return validateJournal(&clone, j.App)
}
