package deploy

import (
	"context"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/engine"
)

func resourceArgs(r *config.Resources) []string {
	return engine.ResourceArgs(r)
}

func (m *Manager) requireResourceSupport(ctx context.Context, r *config.Resources) error {
	return m.engine().RequireResourceSupport(ctx, r)
}

func verifyResources(v *inspection, r *config.Resources) error {
	return engine.VerifyResources(v, r)
}
