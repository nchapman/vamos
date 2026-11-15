package deploy

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
)

// Never discard a started run on the strength of an observation alone. The
// mutation preflight must first make any newly observed result durable.
func (m *Manager) maintenanceRuns(ctx context.Context, j *journal) error {
	entries, err := os.ReadDir(m.dir(j.App))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".run.json") {
			continue
		}
		count++
		if count > 256 {
			return errors.New("run history exceeds maintenance limit")
		}
		r, err := m.loadRun(j, strings.TrimSuffix(entry.Name(), ".run.json"))
		if err != nil {
			return err
		}
		if r.Phase == "building" && m.runContainerAbsent(ctx, r.Container) {
			continue
		}
		if r.Phase == "create-intent" {
			v, e := m.inspect(ctx, "container", r.Container)
			if e == nil && !v.State.Running && !v.State.Restarting && !v.State.Paused && (v.State.StartedAt == "" || strings.HasPrefix(v.State.StartedAt, "0001-")) && m.verifyRun(ctx, j, r, v) == nil {
				continue
			}
			// A resumed removal may already have removed the verified prestart container.
			if m.plannedRunRemoval(j, r) && m.runContainerAbsent(ctx, r.Container) {
				continue
			}
		}
		if r.Phase != "complete" {
			return errors.New("active or unknown run blocks maintenance; a durable completed result is required")
		}
	}
	return nil
}

// A lost removal response may leave the record after its stopped container is
// gone. Only the exact checksummed, captured removal plan authorizes that gap.
func (m *Manager) plannedRunRemoval(j *journal, r *runRecord) bool {
	if j.Maintenance == nil || r.Phase != "create-intent" || r.DockerID == "" {
		return false
	}
	container := false
	for _, c := range j.Maintenance.Containers {
		if c.Name == r.Container && c.ID == r.DockerID && c.Operation == r.Operation {
			container = true
		}
	}
	if !container {
		return false
	}
	for _, f := range j.Maintenance.Files {
		if f.Name != r.Operation+".run.json" {
			continue
		}
		_, sum, err := resourceRead(runPath(m, j, r.Operation), runRecordLimit)
		return err == nil && sum == f.SHA256
	}
	return false
}

func (m *Manager) maintenanceRunContainer(ctx context.Context, j *journal, v *resourceInspection, row resourceRow) error {
	r, err := m.loadRun(j, v.Config.Labels["vamos.run"])
	if err != nil {
		return err
	}
	prestart := r.Phase == "create-intent" && (v.State.StartedAt == "" || strings.HasPrefix(v.State.StartedAt, "0001-"))
	if (r.Phase != "complete" && !prestart) || r.Container != row.Name || r.DockerID != row.ID || v.ID != row.ID || v.Name != "/"+row.Name || !imageID.MatchString(v.Image) || v.State.Running || v.State.Restarting || v.State.Paused {
		return errors.New("run is not a durable stopped maintenance resource")
	}
	iv := v.inspection
	iv.Name = v.Name
	return m.verifyRun(ctx, j, r, &iv)
}

func (m *Manager) emptyUnstartedRunIID(ctx context.Context, j *journal, op, name string) bool {
	r, err := m.loadRun(j, op)
	return err == nil && r.Phase == "building" && r.Config.Build != nil && name == op+"."+imageKey(r.Config)+".image.iid" && m.runContainerAbsent(ctx, r.Container)
}

// Keep the newest 32 terminal results; prune at most 32 per cleanup so the
// persisted plan remains bounded. IID receipts are never pruned by cleanup.
const maintenanceRunRetention = 32
const maintenanceRunPruneBatch = 32

func (m *Manager) maintenanceRunPrune(j *journal) (map[string]bool, error) {
	entries, err := os.ReadDir(m.dir(j.App))
	if err != nil {
		return nil, err
	}
	remove := map[string]bool{}
	var terminal []os.FileInfo
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".run.json") {
			continue
		}
		r, err := m.loadRun(j, strings.TrimSuffix(entry.Name(), ".run.json"))
		if err != nil {
			return nil, err
		}
		if j.Hook != nil && r.Operation == j.Hook.Operation {
			continue
		}
		if r.Phase != "complete" {
			if len(remove) < maintenanceRunPruneBatch {
				remove[entry.Name()] = true
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		terminal = append(terminal, info)
	}
	sort.Slice(terminal, func(i, k int) bool {
		if terminal[i].ModTime().Equal(terminal[k].ModTime()) {
			return terminal[i].Name() < terminal[k].Name()
		}
		return terminal[i].ModTime().Before(terminal[k].ModTime())
	})
	n := len(terminal) - maintenanceRunRetention
	if n > maintenanceRunPruneBatch-len(remove) {
		n = maintenanceRunPruneBatch - len(remove)
	}
	for i := 0; i < n; i++ {
		remove[terminal[i].Name()] = true
	}
	return remove, nil
}
