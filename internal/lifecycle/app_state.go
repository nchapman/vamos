package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nchapman/soar/internal/appstate"
)

// App receipts are the only app state host teardown may discard. Active or
// interrupted operations remain an independent fence, even without a worker.
func (m *Manager) appReceipts(s *State) ([]string, error) {
	root := m.path("/var/lib/soar/apps")
	if err := rejectSymlink(root); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("app state exists without a host ownership manifest")
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || !appstate.ValidName(entry.Name()) {
			return nil, errors.New("unrecognized app state; refusing host mutation")
		}
		dir := filepath.Join(root, entry.Name())
		if err := rejectSymlink(dir); err != nil {
			return nil, err
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		// An interrupted authorized purge may have unlinked a receipt but not its
		// now-empty directory. Removing an empty directory cannot erase app data.
		if len(files) == 0 && s.Phase == "removing" && s.PurgeApproved {
			dirs = append(dirs, dir)
			continue
		}
		if len(files) != 1 || files[0].Name() != "state.json" {
			return nil, fmt.Errorf("app %q has data or incomplete state; finish app destruction first", entry.Name())
		}
		path := filepath.Join(dir, "state.json")
		if err := rejectSymlink(path); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("app receipt is not a regular file")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		_, err = appstate.Decode(f, entry.Name(), s.ID)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("app %q is not a completed destruction receipt: %w", entry.Name(), err)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func (m *Manager) removeAppReceipts(s *State) error {
	dirs, err := m.appReceipts(s)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := os.Remove(filepath.Join(dir, "state.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	root := m.path("/var/lib/soar/apps")
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d, err := os.Open(filepath.Dir(root))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
