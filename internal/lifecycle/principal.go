package lifecycle

import (
	"context"
	"errors"
)

// WithPrincipalACL holds the installation lock across both ACL files, including
// for list, so readers never observe an in-progress grant or revocation.
func (m *Manager) WithPrincipalACL(ctx context.Context, cfg Config, executorSHA string, apply func(string) error) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return err
	}
	if s == nil || s.Phase != "ready" || s.Config != cfg {
		return errors.New("server is not a ready installation with the requested identity")
	}
	if s.ExecutorSHA != executorSHA {
		return errors.New("executor does not match the installed host bundle")
	}
	dir := m.path("/etc/vamos")
	if err := rejectSymlink(dir); err != nil {
		return err
	}
	if err := m.verifyFilesFor(s, true); err != nil {
		return err
	}
	return apply(dir)
}
