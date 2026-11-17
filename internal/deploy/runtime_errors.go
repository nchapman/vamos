package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/nchapman/vamos/internal/engine"
)

func runtimeContext(message string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", message, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
	}
	// Reconstruct the sentinel rather than retaining an arbitrary wrapped error.
	for _, safe := range []error{engine.ErrPortBind, engine.ErrDaemonUnavailable, engine.ErrImageAccess, engine.ErrDiskFull} {
		if errors.Is(err, safe) {
			return fmt.Errorf("%s: %w", message, safe)
		}
	}
	return errors.New(message)
}
