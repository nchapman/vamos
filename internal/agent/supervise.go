package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func workerUnitName(id string) string {
	return "vamos-op-" + id + ".service"
}

func workerStageDir(id string) string {
	return filepath.Join(executor.WorkRoot, id)
}

func stageSupervisedWorker(id string, request deploy.Request, payload []byte) (string, error) {
	if !operationIDRE.MatchString(id) {
		return "", errors.New("invalid operation ID")
	}
	if err := ensurePrivateWorkRoot(executor.WorkRoot); err != nil {
		return "", err
	}
	dir := workerStageDir(id)
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("refusing symlinked worker stage")
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	reqBytes, err := json.Marshal(request)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := writePrivateStageFile(filepath.Join(dir, "request.json"), append(reqBytes, '\n')); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if payloadName := workerPayloadFilename(request); payloadName != "" && len(payload) > 0 {
		if err := writePrivateStageFile(filepath.Join(dir, payloadName), payload); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

func workerPayloadFilename(request deploy.Request) string {
	switch {
	case request.Action == "deploy" || request.Action == "run":
		return "context.tar"
	case request.Action == "secrets-stage" || request.Action == "secrets-set":
		return "secret.value"
	case request.Action == "backup-repository" && request.Backup != nil && request.Backup.NeedsCredentials():
		return "repository.credentials"
	default:
		return ""
	}
}

func ensurePrivateWorkRoot(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s", p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func writePrivateStageFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var (
	startWorkerUnit       = startSupervisedUnit
	queryWorkerUnitActive = isWorkerUnitActive
)

func startSupervisedUnit(ctx context.Context, id, dir string) error {
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{
		"--quiet",
		"--collect",
		"--unit=vamos-op-" + id,
		"--property=RuntimeMaxSec=21min",
		"--property=KillMode=control-group",
		"--property=UMask=0077",
		"--property=NoNewPrivileges=yes",
		"--property=PrivateTmp=yes",
		"--working-directory=" + dir,
		lifecycle.ExecutorPath,
		"internal-agent",
		"worker",
		dir,
	}
	_, err := process.Run(startCtx, "systemd-run", args, nil)
	return err
}

func isWorkerUnitActive(ctx context.Context, unit string) (bool, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(queryCtx, "systemctl", "is-active", unit)
	out, err := cmd.Output()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
			return false, err
		}
		code = exitErr.ExitCode()
	}
	state := strings.TrimSpace(string(out))
	switch fmt.Sprintf("%s:%d", state, code) {
	case "active:0", "reloading:0", "activating:0", "activating:3", "deactivating:0", "deactivating:3":
		return true, nil
	case "inactive:3", "inactive:4", "failed:3", "unknown:3", "unknown:4":
		return false, nil
	default:
		return false, fmt.Errorf("indeterminate systemd state %q (exit %d)", state, code)
	}
}

// runSupervisedWorker launches vamos-op-<id>.service and waits for it to exit.
// If the daemon shuts down while the detached unit is still running, the
// staged directory and running phase are left intact for startup re-adoption.
func (d *Daemon) runSupervisedWorker(ctx context.Context, id string, request deploy.Request, payload []byte) {
	dir, err := stageSupervisedWorker(id, request, payload)
	if err != nil {
		// No worker process started, so report a clean pre-execution failure.
		_ = d.Queue.Finish(id, deploy.Response{}, fmt.Errorf("stage worker: %w", err))
		return
	}
	if err := startWorkerUnit(ctx, id, dir); err != nil {
		// systemd-run may fail before or during unit creation; check whether a
		// unit is active before deciding between clean failure and fencing.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		active, activeErr := queryWorkerUnitActive(checkCtx, workerUnitName(id))
		cancel()
		if activeErr != nil || active {
			_ = d.Queue.Fence(id, "worker launch outcome was indeterminate")
			return
		}
		_ = os.RemoveAll(dir)
		_ = d.Queue.Finish(id, deploy.Response{}, fmt.Errorf("start worker unit: %w", err))
		return
	}
	d.monitorSupervisedUnit(ctx, id, dir)
}

func (d *Daemon) monitorSupervisedUnit(ctx context.Context, id, dir string) {
	unit := workerUnitName(id)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		active, err := queryWorkerUnitActive(ctx, unit)
		if err == nil && !active {
			resp, ok, readErr := executor.ReadWorkerResponse(dir)
			_ = os.RemoveAll(dir)
			if readErr == nil && ok {
				_ = d.Queue.Finish(id, resp, nil)
				return
			}
			_ = d.Queue.Fence(id, "worker exited without a structured response")
			return
		}
		select {
		case <-ctx.Done():
			// Leave an active detached unit running in its own systemd cgroup so
			// the next vamos-agent process can re-adopt or harvest response.json.
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) reconcileSupervisedWorkers(ctx context.Context) (map[string]bool, error) {
	readopted := make(map[string]bool)
	if d.Execute != nil {
		return readopted, nil
	}
	running, err := d.Queue.RunningRequests()
	if err != nil {
		return nil, err
	}
	type activeWorker struct {
		id      string
		dir     string
		req     deploy.Request
		release func()
	}
	var workers []activeWorker
	for id, req := range running {
		dir := workerStageDir(id)
		active, activeErr := queryWorkerUnitActive(ctx, workerUnitName(id))
		if activeErr == nil && active {
			readopted[id] = true
			releaseSlots, _ := d.tryAcquireOperationSlots(ctx, req, false)
			workers = append(workers, activeWorker{id: id, dir: dir, req: req, release: releaseSlots})
			continue
		}
		if activeErr == nil && !active {
			if resp, ok, readErr := executor.ReadWorkerResponse(dir); readErr == nil && ok {
				_ = os.RemoveAll(dir)
				if err := d.Queue.Finish(id, resp, nil); err != nil {
					for _, w := range workers {
						if w.release != nil {
							w.release()
						}
					}
					return nil, err
				}
				readopted[id] = true
				continue
			}
			_ = os.RemoveAll(dir)
		}
	}
	for _, w := range workers {
		d.workWG.Add(1)
		go func() {
			defer d.workWG.Done()
			releaseSlots := w.release
			if releaseSlots == nil {
				var slotErr error
				releaseSlots, slotErr = d.acquireOperationSlots(ctx, w.req)
				if slotErr != nil {
					return
				}
			}
			defer releaseSlots()
			d.monitorSupervisedUnit(ctx, w.id, w.dir)
		}()
	}
	return readopted, nil
}
