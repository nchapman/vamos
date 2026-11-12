package lifecycle

import (
	"errors"
	"os"

	"github.com/nchapman/soar/internal/volumes"
)

// Registry existence alone fences identity replacement and purge, even when
// Docker has lost every recorded volume. Never erase ownership evidence.
func (m *Manager) rejectRetainedVolumes() error {
	path := m.path(volumes.Dir)
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("retained volume registry prevents host identity replacement or teardown; volume provenance must be retained")
	}
	return nil
}
