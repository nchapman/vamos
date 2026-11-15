package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/config"
)

// A hook belongs to one captured deployment. It is not inherited by later
// manifests and is never copied into a rollback or service runtime snapshot.
type releaseCapture struct {
	Operation string
	Directive config.Release
	ExitCode  *int `json:",omitempty"`
}

func validRunVersion(r *runRecord) bool {
	if r.Version == 1 {
		return r.Purpose == "" && r.ParentOperation == ""
	}
	return r.Version == 2 && r.Purpose == "release" && operation.MatchString(r.ParentOperation) && r.ParentOperation != r.Operation && r.Phase != "building"
}

func captureRelease(j *journal, directive *config.Release) error {
	if directive == nil {
		return nil
	}
	slot := j.ServiceStates[directive.Service]
	if slot == nil || slot.Skipped || !slices.Contains(j.Selected, directive.Service) {
		return errors.New("release runner must be a selected, non-skipped service")
	}
	digest := sha256.Sum256([]byte("vamos-release/" + j.App + "/" + j.Operation))
	j.Hook = &releaseCapture{Operation: hex.EncodeToString(digest[:16]), Directive: config.Release{Service: directive.Service, Command: slices.Clone(directive.Command)}}
	j.Version = 8
	// Desired runtime state deliberately excludes deployment directives.
	copied := *j.Config
	copied.Release = nil
	j.Config = &copied
	return nil
}

func validateReleaseCapture(j *journal) error {
	bad := errors.New("invalid captured release command")
	if j.Release != nil || j.Config.Release != nil {
		return bad
	}
	h := j.Hook
	if h == nil {
		if j.Phase == "releasing" {
			return bad
		}
		return nil
	}
	if j.Version != 8 || j.Rollback != nil || !operation.MatchString(h.Operation) || h.Operation == j.Operation {
		return bad
	}
	slot := j.ServiceStates[h.Directive.Service]
	if slot == nil || slot.Skipped || !slices.Contains(j.Selected, h.Directive.Service) {
		return bad
	}
	cfg := *j.Config
	cfg.Release = &h.Directive
	if cfg.Validate() != nil {
		return bad
	}
	if h.ExitCode != nil && (*h.ExitCode < 0 || *h.ExitCode > 255) {
		return bad
	}
	if j.Phase == "building" && h.ExitCode != nil {
		return bad
	}
	if j.Phase != "building" && j.Phase != "releasing" && (h.ExitCode == nil || *h.ExitCode != 0) {
		return bad
	}
	return nil
}

func (m *Manager) hookRecord(j *journal) *runRecord {
	h := j.Hook
	cfg := j.Config.Services[h.Directive.Service]
	cfg.Command = slices.Clone(h.Directive.Command)
	cfg.HTTP, cfg.Volume = nil, nil
	return &runRecord{Version: 2, App: j.App, HostID: j.HostID,
		RunResult: RunResult{Operation: h.Operation, Service: h.Directive.Service, Phase: "create-intent", Container: "vamos-run-" + h.Operation, Image: selectedImage(j, h.Directive.Service), Purpose: "release", ParentOperation: j.Operation},
		Config:    cfg, SecretRefs: j.ServiceStates[h.Directive.Service].SecretRefs}
}

func matchingHookRecord(want, got *runRecord) bool {
	return got.Version == 2 && got.App == want.App && got.HostID == want.HostID &&
		got.Operation == want.Operation && got.ParentOperation == want.ParentOperation &&
		got.Purpose == "release" && got.Service == want.Service && got.Container == want.Container &&
		got.Image == want.Image && reflect.DeepEqual(got.Config, want.Config) && maps.Equal(got.SecretRefs, want.SecretRefs)
}

// Keep the same bounded execution boundary as ordinary runs. Creation is safe
// to reconcile before start intent. Once start intent is saved, never send
// another start, even when the container appears not to have started.
func (m *Manager) advanceRelease(ctx context.Context, j *journal) error {
	if j.Hook == nil {
		return nil
	}
	expected := m.hookRecord(j)
	if err := m.requireResourceSupport(ctx, expected.Config.Resources); err != nil {
		return err
	}
	im, err := m.inspect(ctx, "image", expected.Image)
	if err != nil || im.ID != expected.Image || imageVolumesAllowed(im, nil, false) != nil {
		return errors.New("release command requires a captured image without declared volumes")
	}
	if err = m.ensureNetwork(ctx, j); err != nil {
		return err
	}
	rec, err := m.loadRun(j, expected.Operation)
	if errors.Is(err, os.ErrNotExist) {
		paths, scanErr := filepath.Glob(filepath.Join(m.dir(j.App), "*.run.json"))
		if scanErr != nil || len(paths) >= 256 {
			return errors.New("run history full; explicit cleanup required before release command")
		}
		iid := filepath.Join(m.dir(j.App), expected.Operation+"."+imageKey(expected.Config)+".image.iid")
		// This receipt prevents ordinary run IDs being reused after result pruning.
		if data, _, readErr := resourceRead(iid, 128); readErr == nil {
			if string(data) != expected.Image+"\n" {
				return errors.New("release image receipt mismatch")
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return errors.New("release image receipt unavailable")
		} else if err = atomic(iid, []byte(expected.Image+"\n"), 0600); err != nil {
			return err
		}
		rec = expected
		if err = m.saveRun(j, rec); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !matchingHookRecord(expected, rec) {
		return errors.New("release execution does not match captured inputs")
	}
	if rec.Phase == "create-intent" {
		exists, e := m.exists(ctx, "container", rec.Container)
		if e != nil {
			return errors.New("inspect release container presence failed")
		}
		if !exists {
			if rec.DockerID != "" {
				return errors.New("recorded release container missing; execution will not be retried")
			}
			args := runCreateArgs(j, rec)
			if m.runContainer(ctx, j, &release{Image: rec.Image, SecretRefs: rec.SecretRefs}, args) != nil {
				return errors.New("release create outcome unknown; resume to inspect before execution")
			}
		}
		v, e := m.inspect(ctx, "container", rec.Container)
		if e != nil || (rec.DockerID != "" && rec.DockerID != v.ID) {
			return errors.New("release container identity unavailable")
		}
		rec.DockerID = v.ID
		if len(v.ID) != 64 || m.verifyRun(ctx, j, rec, v) != nil || v.State.Running || v.State.Restarting || v.State.Paused || (v.State.StartedAt != "" && !strings.HasPrefix(v.State.StartedAt, "0001-")) {
			return errors.New("release container is unsafe or may already have executed")
		}
		rec.Phase = "start-intent"
		if err = m.saveRun(j, rec); err != nil {
			return err
		}
		// The result is determined by observation, not the start RPC acknowledgement.
		_, _ = m.cmd(ctx, "docker", "start", rec.DockerID)
	}
	observed := m.observeRun(ctx, j, rec)
	if observed.Phase == "running" {
		_, _ = m.cmd(ctx, "docker", "wait", rec.DockerID)
		observed = m.observeRun(ctx, j, rec)
	}
	if observed.Phase != "complete" || observed.ExitCode == nil {
		return errors.New("release command is running or its outcome is unknown; inspect its run and resume observation; it will not be retried")
	}
	rec.RunResult = *observed
	if err = m.saveRun(j, rec); err != nil {
		return errors.New("release result persistence failed; resume observation, not execution")
	}
	// The execution result is durable before the parent gate can open.
	code := *rec.ExitCode
	if j.Hook.ExitCode != nil && *j.Hook.ExitCode != code {
		return errors.New("release outcome contradicts captured result")
	}
	j.Hook.ExitCode = &code
	if err = m.save(j); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("release command failed with exit code %d; rollout blocked; inspect logs before explicitly superseding", code)
	}
	return nil
}

func runCreateArgs(j *journal, rec *runRecord) []string {
	args := []string{"create", "--name", rec.Container, "--restart", "no", "--network", network(j), "--label", "vamos.host=" + j.HostID, "--label", "vamos.app=" + j.App, "--label", "vamos.operation=" + rec.Operation, "--label", "vamos.service=" + rec.Service, "--label", "vamos.run=" + rec.Operation}
	if rec.Purpose == "release" {
		args = append(args, "--label", "vamos.release="+rec.ParentOperation)
	}
	args = append(args, resourceArgs(rec.Config.Resources)...)
	keys := make([]string, 0, len(rec.Config.Env))
	for k := range rec.Config.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+rec.Config.Env[k])
	}
	args = append(args, rec.Image)
	return append(args, rec.Config.Command...)
}

func (m *Manager) releaseStatus(ctx context.Context, j *journal) *RunResult {
	if j.Hook == nil {
		return nil
	}
	result, err := m.runStatus(ctx, j, j.Hook.Operation)
	if err != nil {
		phase := "unknown"
		if j.Phase == "building" && errors.Is(err, os.ErrNotExist) {
			phase = "not-started"
		}
		return &RunResult{Operation: j.Hook.Operation, Service: j.Hook.Directive.Service, Phase: phase, Purpose: "release", ParentOperation: j.Operation}
	}
	result.Output = ""
	return result
}
