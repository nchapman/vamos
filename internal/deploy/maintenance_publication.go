package deploy

import (
	"errors"
	"fmt"
	"os"
)

func maintenanceCompleted(j *journal, h *maintenanceManifestHeader) bool {
	return (j.LastMaintenanceOperation == h.Intent.Operation && j.LastMaintenanceAction == h.Intent.Action) || (j.Phase == "destroyed" && j.Operation == h.Intent.Operation && h.Intent.Action == "destroy")
}

// Execute calls the fence under the host lock, before any mutating dispatch.
func (m *Manager) fenceMaintenancePublication(j *journal, r Request) error {
	dir := m.maintenanceManifestDir(j.App)
	if err := safe(dir); err != nil {
		return err
	}
	_, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	h, err := m.maintenanceHeader(j)
	if errors.Is(err, os.ErrNotExist) {
		return m.discardUnpublishedMaintenance(j)
	}
	if err != nil {
		return err
	}
	if maintenanceCompleted(j, h) {
		return m.discardMaintenanceStorage(j, h)
	}
	basis, err := maintenanceBasis(j)
	if err != nil {
		return err
	}
	if h.Basis != basis {
		return errors.New("orphan maintenance authority differs from deployment journal")
	}
	if r.App != j.App || r.Operation != h.Intent.Operation || (r.Action != "resume" && r.Action != h.Intent.Action) {
		return fmt.Errorf("prepared maintenance requires exact operation recovery: app %s, operation %s", j.App, h.Intent.Operation)
	}
	return nil
}

// A complete published manifest can outlive a lost state.json installation.
// Exact-operation resume adopts it rather than discovering new work. An
// interrupted, explicitly Preparing publication grants no deletion authority.
func (m *Manager) recoverMaintenancePublication(j *journal, r *Request) error {
	if j == nil || j.Maintenance != nil || r.Action != "resume" {
		return nil
	}
	h, err := m.maintenanceHeader(j)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if maintenanceCompleted(j, h) {
		return nil
	}
	if h.Intent.Operation != r.Operation {
		return errors.New("prepared maintenance operation differs")
	}
	r.Action = h.Intent.Action
	r.Service = h.Intent.Service
	r.Apply = true
	// The preparing/publication header exists only after the original protected
	// service authorization gate. It never changes the captured service/config.
	r.AllowProtected = true
	return nil
}
