package deploy

import (
	"context"
	"errors"
)

// Once provenance deletion begins, resource deletion must never be retried.
// However, a replacement must still prevent completion, not be silently adopted
// or removed using the old operation. This check needs no discarded IID proof.
func (m *Manager) verifyMaintenanceResourceBarrier(ctx context.Context, w *maintenance) error {
	if len(w.Containers) > 0 {
		rows, err := m.resourceList(ctx, "container")
		if err != nil {
			return err
		}
		names := map[string]bool{}
		ids := map[string]bool{}
		for _, c := range w.Containers {
			names[c.Name] = true
			ids[c.ID] = true
		}
		for _, r := range rows {
			if names[r.Name] || ids[r.ID] {
				return errors.New("container exists after maintenance resource barrier")
			}
		}
	}
	if w.Network != nil {
		rows, err := m.resourceList(ctx, "network")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Name == w.Network.Name || r.ID == w.Network.ID {
				return errors.New("network exists after maintenance resource barrier")
			}
		}
	}
	return nil
}
