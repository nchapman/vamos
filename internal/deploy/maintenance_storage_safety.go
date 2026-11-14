package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) checkMaintenanceNamespace() error {
	root := filepath.Dir(m.maintenanceManifestDir("placeholder"))
	if err := safe(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unknown maintenance namespace entry")
		}
		if _, err := m.load(entry.Name()); err != nil {
			return errors.New("maintenance storage without valid app journal")
		}
	}
	return m.removeEmptyMaintenanceRoot()
}
func (m *Manager) removeEmptyMaintenanceRoot() error {
	root := filepath.Dir(m.maintenanceManifestDir("placeholder"))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	if err = os.Remove(root); err != nil {
		return err
	}
	return syncResourceDir(filepath.Dir(root))
}

// Atomic publication can leave a temporary file if the process dies before the
// initial preparing header is renamed. No pages can have been written yet and
// no journal reference can exist. Anything beyond these narrowly named regular
// temporary files is unexplained authority and must fail closed.
func (m *Manager) discardUnpublishedMaintenance(j *journal) error {
	dir := m.maintenanceManifestDir(j.App)
	if err := safe(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(dir, "intent.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("maintenance publication unexpectedly present")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Name(), ".deploy-") || !info.Mode().IsRegular() {
			return errors.New("unexplained maintenance files without publication header")
		}
	}
	for _, entry := range entries {
		if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err = syncResourceDir(dir); err != nil {
		return err
	}
	if err = os.Remove(dir); err != nil {
		return err
	}
	if err = syncResourceDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return m.removeEmptyMaintenanceRoot()
}
