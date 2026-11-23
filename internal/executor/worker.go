package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

const (
	WorkRoot = "/run/vamos/work"
	LockDir  = "/run/lock/vamos"
)

var workerIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ServeWorker executes one supervised mutating operation staged by vamos-agent
// inside its own transient systemd unit (vamos-op-<id>.service). Writing a
// complete response.json establishes a definitive worker outcome; if the unit
// crashes or receives SIGTERM/SIGKILL before that rename, no response file is
// published and vamos-agent fences the operation as uncertain.
func ServeWorker(ctx context.Context, workDir string) int {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return 1
	}
	return serveWorkerInRoot(ctx, "/", WorkRoot, workDir)
}

func serveWorkerInRoot(ctx context.Context, root, expectedParent, workDir string) int {
	cleanDir := filepath.Clean(workDir)
	id := filepath.Base(cleanDir)
	if filepath.Dir(cleanDir) != filepath.Clean(expectedParent) || !workerIDPattern.MatchString(id) {
		return 1
	}
	if err := validatePrivateDir(expectedParent, 0700); err != nil {
		return 1
	}
	if err := validatePrivateDir(cleanDir, 0700); err != nil {
		return 1
	}

	reqPath := filepath.Join(cleanDir, "request.json")
	reqFile, err := openPrivateRegular(reqPath, maxAppRequest)
	if err != nil {
		return 1
	}
	request, err := readAppRequest(reqFile)
	reqFile.Close()
	if err != nil {
		return 1
	}
	if err := deploy.ValidateRequest(request); err != nil || !request.Mutating() {
		return 1
	}

	archive, payloadErr := loadWorkerPayload(cleanDir, request)
	if payloadErr != nil {
		if ctx.Err() != nil {
			return 1
		}
		resp := FormatAppResponse(request, deploy.Response{}, payloadErr)
		if writeWorkerResponse(cleanDir, resp) != nil {
			return 1
		}
		return 1
	}

	if root == "/" {
		if err := os.Chdir(cleanDir); err != nil {
			return 1
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	manager := deploy.NewManager()
	manager.Root = root
	manager.RuntimeDir = cleanDir
	manager.MutationLock = func(_ context.Context, app string, mutating bool) (bool, func(), error) {
		return acquireCoordinatedMutation(root, request, app, mutating)
	}
	manager.IngressLock = func(lockCtx context.Context) (func(), error) {
		return acquireIngressLock(lockCtx, root)
	}

	rawResp, runErr := manager.Execute(execCtx, request, archive)
	// External unit/process termination must leave the queue operation
	// uncertain rather than claiming a definitive failure mid-flight.
	if ctx.Err() != nil {
		return 1
	}
	formatted := FormatAppResponse(request, rawResp, runErr)
	if err := writeWorkerResponse(cleanDir, formatted); err != nil {
		return 1
	}
	if runErr != nil || formatted.Error != "" {
		return 1
	}
	return 0
}

// ExecuteRead performs an authorized non-mutating request directly inside
// vamos-agent without persisting queue records or spawning a transient unit,
// while probing the same kernel locks workers hold to report status.Active.
func ExecuteRead(ctx context.Context, request deploy.Request) (deploy.Response, error) {
	return ExecuteReadInRoot(ctx, "/", request)
}

func ExecuteReadInRoot(ctx context.Context, root string, request deploy.Request) (deploy.Response, error) {
	if err := deploy.ValidateRequest(request); err != nil {
		return deploy.Response{}, err
	}
	if request.Mutating() {
		return deploy.Response{}, errors.New("ExecuteRead requires a non-mutating request")
	}
	runtimeDir, err := os.MkdirTemp("", "vamos-read-*")
	if err != nil {
		return deploy.Response{}, err
	}
	defer os.RemoveAll(runtimeDir)

	execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	manager := deploy.NewManager()
	manager.Root = root
	manager.RuntimeDir = runtimeDir
	manager.MutationLock = func(_ context.Context, app string, _ bool) (bool, func(), error) {
		active, err := ProbeMutationActive(root, app)
		return active, func() {}, err
	}
	manager.IngressLock = func(lockCtx context.Context) (func(), error) {
		return acquireIngressLock(lockCtx, root)
	}
	rawResp, runErr := manager.Execute(execCtx, request, nil)
	return FormatAppResponse(request, rawResp, runErr), nil
}

// ReadWorkerResponse loads and validates a completed worker's response.json.
func ReadWorkerResponse(workDir string) (deploy.Response, bool, error) {
	respPath := filepath.Join(workDir, "response.json")
	if _, err := os.Lstat(respPath); errors.Is(err, os.ErrNotExist) {
		return deploy.Response{}, false, nil
	}
	f, err := openPrivateRegular(respPath, maxAppRequest)
	if err != nil {
		return deploy.Response{}, false, errors.New("invalid worker response file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxAppRequest+1))
	if err != nil {
		return deploy.Response{}, false, err
	}
	if len(data) > maxAppRequest {
		return deploy.Response{}, false, errors.New("worker response exceeds maximum size")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var resp deploy.Response
	if err := dec.Decode(&resp); err != nil {
		return deploy.Response{}, false, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return deploy.Response{}, false, errors.New("trailing worker response data")
	}
	if resp.Version != 1 {
		return deploy.Response{}, false, errors.New("unsupported worker response version")
	}
	return resp, true, nil
}

func loadWorkerPayload(workDir string, request deploy.Request) ([]byte, error) {
	switch {
	case request.Action == "deploy" || request.Action == "run":
		path := filepath.Join(workDir, "context.tar")
		defer os.Remove(path)
		f, err := openPrivateRegular(path, buildcontext.MaxBytes)
		if err != nil {
			return nil, errors.New("context.tar must be a regular private file within the context size limit")
		}
		defer f.Close()
		return readAppContext(f)
	case request.Action == "secrets-stage" || request.Action == "secrets-set":
		path := filepath.Join(workDir, "secret.value")
		defer os.Remove(path)
		return readSecretsFile(path, request)
	case request.Action == "backup-repository" && request.Backup.NeedsCredentials():
		path := filepath.Join(workDir, "repository.credentials")
		defer os.Remove(path)
		return readRepositoryFile(path)
	default:
		return nil, nil
	}
}

func writeWorkerResponse(workDir string, resp deploy.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(workDir, ".response-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(workDir, "response.json")); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(workDir)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

func acquireCoordinatedMutation(root string, request deploy.Request, app string, mutating bool) (bool, func(), error) {
	if !mutating {
		active, err := ProbeMutationActive(root, app)
		return active, func() {}, err
	}
	hostPath, coordDir, err := prepareLockPaths(root)
	if err != nil {
		return false, nil, err
	}
	// Shared host lock prevents host upgrade/teardown (which take an exclusive
	// lock on vamos-host.lock) while allowing concurrent multi-app workers.
	hostLock := flock.New(hostPath, flock.SetPermissions(0600))
	ok, err := hostLock.TryRLock()
	if err != nil {
		_ = hostLock.Close()
		return false, nil, err
	}
	if !ok {
		_ = hostLock.Close()
		return true, nil, nil
	}

	exclusiveHostOp := app == "" || request.Action == "destroy" || request.Action == "service-remove"
	hostOpLock := flock.New(filepath.Join(coordDir, "host-op.lock"), flock.SetPermissions(0600))
	if exclusiveHostOp {
		ok, err = hostOpLock.TryLock()
	} else {
		ok, err = hostOpLock.TryRLock()
	}
	if err != nil {
		_ = hostOpLock.Close()
		_ = hostLock.Unlock()
		_ = hostLock.Close()
		return false, nil, err
	}
	if !ok {
		_ = hostOpLock.Close()
		_ = hostLock.Unlock()
		_ = hostLock.Close()
		return true, nil, nil
	}

	var appLock *flock.Flock
	if app != "" {
		appLock = flock.New(filepath.Join(coordDir, "app-"+app+".lock"), flock.SetPermissions(0600))
		ok, err = appLock.TryLock()
		if err != nil {
			_ = appLock.Close()
			_ = hostOpLock.Unlock()
			_ = hostOpLock.Close()
			_ = hostLock.Unlock()
			_ = hostLock.Close()
			return false, nil, err
		}
		if !ok {
			_ = appLock.Close()
			_ = hostOpLock.Unlock()
			_ = hostOpLock.Close()
			_ = hostLock.Unlock()
			_ = hostLock.Close()
			return true, nil, nil
		}
	}

	unlock := func() {
		if appLock != nil {
			_ = appLock.Unlock()
			_ = appLock.Close()
		}
		_ = hostOpLock.Unlock()
		_ = hostOpLock.Close()
		_ = hostLock.Unlock()
		_ = hostLock.Close()
	}
	return false, unlock, nil
}

// ProbeMutationActive checks whether an exclusive host operation, host-wide
// maintenance operation, or per-app mutation is currently holding its kernel lock.
func ProbeMutationActive(root, app string) (bool, error) {
	hostPath := filepath.Join(root, lifecycle.LockPath)
	if err := rejectSymlinkAncestors(hostPath); err != nil {
		return false, err
	}
	if _, statErr := os.Lstat(hostPath); statErr == nil {
		hostLock := flock.New(hostPath, flock.SetPermissions(0600))
		ok, err := hostLock.TryRLock()
		if err != nil {
			_ = hostLock.Close()
			return false, err
		}
		if !ok {
			_ = hostLock.Close()
			return true, nil
		}
		_ = hostLock.Unlock()
		_ = hostLock.Close()
	}

	coordDir := filepath.Join(root, LockDir)
	if err := rejectSymlinkAncestors(coordDir); err != nil {
		return false, err
	}
	hostOpPath := filepath.Join(coordDir, "host-op.lock")
	if _, statErr := os.Lstat(hostOpPath); statErr == nil {
		hostOpLock := flock.New(hostOpPath, flock.SetPermissions(0600))
		ok, err := hostOpLock.TryRLock()
		if err != nil {
			_ = hostOpLock.Close()
			return false, err
		}
		if !ok {
			_ = hostOpLock.Close()
			return true, nil
		}
		_ = hostOpLock.Unlock()
		_ = hostOpLock.Close()
	}

	if app != "" {
		appPath := filepath.Join(coordDir, "app-"+app+".lock")
		if _, statErr := os.Lstat(appPath); statErr == nil {
			appLock := flock.New(appPath, flock.SetPermissions(0600))
			ok, err := appLock.TryRLock()
			if err != nil {
				_ = appLock.Close()
				return false, err
			}
			if !ok {
				_ = appLock.Close()
				return true, nil
			}
			_ = appLock.Unlock()
			_ = appLock.Close()
		}
	}
	return false, nil
}

func acquireIngressLock(ctx context.Context, root string) (func(), error) {
	_, coordDir, err := prepareLockPaths(root)
	if err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(coordDir, "ingress.lock"), flock.SetPermissions(0600))
	ok, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !ok {
		_ = lock.Close()
		return nil, errors.New("could not acquire Caddy ingress lock")
	}
	return func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}, nil
}

func prepareLockPaths(root string) (string, string, error) {
	hostPath := filepath.Join(root, lifecycle.LockPath)
	if err := rejectSymlinkAncestors(hostPath); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0755); err != nil {
		return "", "", err
	}
	coordDir := filepath.Join(root, LockDir)
	if err := rejectSymlinkAncestors(coordDir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(coordDir, 0700); err != nil {
		return "", "", err
	}
	return hostPath, coordDir, nil
}

func rejectSymlinkAncestors(target string) error {
	for p := filepath.Clean(target); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s", p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func validatePrivateDir(path string, mode os.FileMode) error {
	if err := rejectSymlinkAncestors(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return fmt.Errorf("directory %s must be private mode %04o", path, mode)
	}
	return nil
}

func openPrivateRegular(path string, maxSize int64) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 || info.Size() > maxSize {
		return nil, fmt.Errorf("invalid private file %s", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		f.Close()
		return nil, fmt.Errorf("private file %s changed", filepath.Base(path))
	}
	return f, nil
}
