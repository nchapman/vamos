package lifecycle

import (
	"context"
	"errors"

	"github.com/nchapman/vamos/internal/buildkit"
	"github.com/nchapman/vamos/internal/process"
)

func (m *Manager) buildService() buildkit.Manager {
	return buildkit.Manager{Root: m.Root, Run: func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if len(input) != 0 {
			return process.Result{}, errors.New("host teardown cannot submit a build")
		}
		return m.Run(ctx, name, args)
	}}
}
