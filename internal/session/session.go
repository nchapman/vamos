// Package session stages short-lived, single-use interactive Docker sessions.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
)

const (
	preparationTTL = 2 * time.Minute
	maxSessions    = 16
	maxExecutable  = 64 << 20
	maxMetadata    = 128 << 10
)

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var errRecordData = errors.New("invalid or oversized session record data")

// Preparation is the bounded response to a preparation request.
type Preparation struct {
	Version int                  `json:"version"`
	Token   string               `json:"token"`
	Target  deploy.SessionTarget `json:"target"`
	Expires time.Time            `json:"expires"`
	Error   string               `json:"error,omitempty"`
}

type record struct {
	Preparation
	Request deploy.SessionRequest `json:"request"`
}

var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidTarget confirms a preparation describes exactly the requested session:
// same app/service/candidate, well-formed identities, and matching local
// service bindings. Both the direct executor transport and the coordinator
// agent enforce it before attaching a command.
func ValidTarget(r deploy.SessionRequest, t deploy.SessionTarget) bool {
	if t.App != r.App || t.Service == "" || (r.Service != "" && t.Service != r.Service) || t.Candidate != r.Candidate || !tokenPattern.MatchString(t.HostIdentity) || !tokenPattern.MatchString(t.Incarnation) || !tokenPattern.MatchString(t.Operation) || !containerIDPattern.MatchString(t.ContainerID) {
		return false
	}
	binding, ok := r.Bindings[t.Service]
	if ok && (binding.HostIdentity != t.HostIdentity || binding.Incarnation != t.Incarnation) {
		return false
	}
	return true
}

// WindowSize describes terminal dimensions for interactive PTY sessions.
type WindowSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Manager's hooks allow tests to resolve and stream without a Docker daemon.
// Configure a Manager before use; its fields must not change during a session.
type Manager struct {
	Root    string
	Now     func() time.Time
	Resolve func(context.Context, deploy.SessionRequest) (deploy.SessionTarget, error)
	Run     func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error)
	RunPTY  func(context.Context, string, []string, *WindowSize, <-chan WindowSize, io.Reader, io.Writer) (int, error)
}

func NewManager() *Manager {
	return &Manager{Root: "/run/vamos-sessions", Now: time.Now,
		Resolve: deploy.NewManager().ResolveSession, Run: process.Stream, RunPTY: runPTY}
}

func (m *Manager) defaults() Manager {
	n := *m
	if n.Root == "" {
		n.Root = "/run/vamos-sessions"
	}
	if n.Now == nil {
		n.Now = time.Now
	}
	if n.Resolve == nil {
		n.Resolve = deploy.NewManager().ResolveSession
	}
	if n.Run == nil {
		n.Run = process.Stream
	}
	if n.RunPTY == nil {
		n.RunPTY = runPTY
	}
	return n
}

// Prepare copies the executable supplied by the entrypoint (os.Executable),
// captures admission, and atomically publishes a two-minute preparation. The
// entrypoint, not a remote request, selects executable.
func (m *Manager) Prepare(ctx context.Context, token string, req deploy.SessionRequest, executable string) (p Preparation, err error) {
	n := m.defaults()
	p = Preparation{Version: 1, Token: token}
	defer func() {
		if err != nil {
			p.Error = err.Error()
		}
	}()
	if err = validToken(token); err != nil {
		return p, err
	}
	if err = deploy.ValidateSessionRequest(req); err != nil {
		return p, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return p, err
	}
	root, err := openRoot(n.Root, true)
	if err != nil {
		return p, err
	}
	defer root.Close()
	lock, err := lockFile(root, ".lock", true)
	if err != nil {
		return p, err
	}
	defer lock.Close()
	count, err := n.prune(root)
	if err != nil {
		return p, err
	}
	if count >= maxSessions {
		return p, errors.New("session capacity reached")
	}
	if err = root.Mkdir(token, 0700); err != nil {
		return p, err
	}
	dir, err := openDir(root, token)
	if err != nil {
		_ = root.Remove(token)
		return p, err
	}
	defer dir.Close()
	defer func() {
		if err != nil {
			err = errors.Join(err, removeStaging(root, token, dir))
		}
	}()
	sessionLock, err := lockFile(dir, "lock", true)
	if err != nil {
		return p, err
	}
	defer sessionLock.Close()
	if err = copyExecutable(dir, executable); err != nil {
		return p, err
	}
	p.Target, err = n.Resolve(ctx, req)
	if err != nil {
		return p, err
	}
	if err = ctx.Err(); err != nil {
		return p, err
	}
	p.Expires = n.Now().Add(preparationTTL)
	data, err := json.Marshal(record{Preparation: p, Request: req})
	if err != nil {
		return p, err
	}
	if len(data) > maxMetadata {
		return p, errors.New("session metadata too large")
	}
	if err = writeFile(dir, "record.tmp", data, 0600); err != nil {
		return p, err
	}
	if err = dir.Rename("record.tmp", "prepared.json"); err != nil {
		return p, err
	}
	return p, nil
}

// Attach consumes a preparation before re-admission. No root or host mutation
// lock is held while streaming. Nonzero workload exits are returned unchanged.
func (m *Manager) Attach(ctx context.Context, token string, stdin io.Reader, stdout, stderr io.Writer) (code int, err error) {
	return m.attach(ctx, token, false, nil, nil, stdin, stdout, stderr)
}

// AttachAgent consumes a preparation on behalf of vamos-agent, allocating a
// host PTY for TTY sessions because the restricted SSH bridge runs pipe-only.
func (m *Manager) AttachAgent(ctx context.Context, token string, initial *WindowSize, resizes <-chan WindowSize, stdin io.Reader, stdout, stderr io.Writer) (code int, err error) {
	return m.attach(ctx, token, true, initial, resizes, stdin, stdout, stderr)
}

func (m *Manager) attach(ctx context.Context, token string, allocatePTY bool, initial *WindowSize, resizes <-chan WindowSize, stdin io.Reader, stdout, stderr io.Writer) (code int, err error) {
	n := m.defaults()
	if err = validToken(token); err != nil {
		return -1, err
	}
	root, err := openRoot(n.Root, false)
	if err != nil {
		return -1, err
	}
	defer root.Close()
	rootLock, err := lockFile(root, ".lock", false)
	if err != nil {
		return -1, err
	}
	defer rootLock.Close()
	dir, err := openDir(root, token)
	if err != nil {
		return -1, err
	}
	defer dir.Close()
	lock, err := lockFile(dir, "lock", false)
	if err != nil {
		return -1, err
	}
	defer lock.Close()
	// Once exclusively owned, even failed admission consumes and removes staging.
	defer func() { err = errors.Join(err, removeStaging(root, token, dir)) }()
	if _, e := dir.Lstat("claimed.json"); !errors.Is(e, os.ErrNotExist) {
		return -1, errors.New("session already claimed")
	}
	rec, err := readRecord(dir, "prepared.json")
	if err != nil {
		return -1, err
	}
	if rec.Version != 1 || rec.Token != token || rec.Error != "" {
		return -1, errors.New("invalid session record")
	}
	if !n.Now().Before(rec.Expires) {
		return -1, errors.New("session preparation expired")
	}
	if err = deploy.ValidateSessionRequest(rec.Request); err != nil {
		return -1, err
	}
	if err = checkFile(dir, "executor", 0700); err != nil {
		return -1, err
	}
	if err = dir.Rename("prepared.json", "claimed.json"); err != nil {
		return -1, err
	}
	if err = rootLock.Close(); err != nil {
		return -1, err
	}
	if err = ctx.Err(); err != nil {
		return -1, err
	}
	target, err := n.Resolve(ctx, rec.Request)
	if err != nil {
		return -1, err
	}
	if target != rec.Target {
		return -1, errors.New("session target changed since preparation")
	}
	if err = ctx.Err(); err != nil {
		return -1, err
	}
	req := rec.Request
	var args []string
	if req.Action == "exec" {
		args = []string{"exec"}
		if req.Stdin {
			args = append(args, "-i")
		}
		if req.TTY {
			args = append(args, "-t")
		}
		args = append(args, target.ContainerID)
		args = append(args, req.Command...)
	} else {
		args = []string{"logs", "--timestamps", "--tail", strconv.Itoa(req.Tail), "--follow", target.ContainerID}
	}
	if !req.Stdin {
		stdin = nil
	}
	if allocatePTY && req.TTY {
		return n.RunPTY(ctx, "docker", args, initial, resizes, stdin, stdout)
	}
	return n.Run(ctx, "docker", args, stdin, stdout, stderr)
}

// Cleanup only removes an owned, inactive token directory. It never waits for
// an active attachment, follows a staging symlink, or recursively removes data.
func (m *Manager) Cleanup(token string) error {
	n := m.defaults()
	if err := validToken(token); err != nil {
		return err
	}
	root, err := openRoot(n.Root, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := lockFile(root, ".lock", false)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir, err := openDir(root, token)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	sl, err := lockFile(dir, "lock", false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if sl != nil {
		defer sl.Close()
	}
	return removeStaging(root, token, dir)
}

func validToken(token string) error {
	if !tokenPattern.MatchString(token) {
		return errors.New("session token must be 32 lowercase hexadecimal characters")
	}
	return nil
}

func openRoot(path string, create bool) (*os.Root, error) {
	if create {
		// Do not create or change permissions on arbitrary ancestors.
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = private(info, true, 0700); err != nil {
		return nil, fmt.Errorf("session root: %w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, errors.New("session root changed")
	}
	return root, nil
}

func openDir(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = private(info, true, 0700); err != nil {
		return nil, err
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		dir.Close()
		return nil, errors.New("session directory changed")
	}
	return dir, nil
}

func private(info os.FileInfo, directory bool, mode os.FileMode) error {
	if !owned(info) || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != mode || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return errors.New("session path must be privately owned with restrictive permissions and no symlinks")
	}
	return nil
}

func checkFile(dir *os.Root, name string, mode os.FileMode) error {
	info, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	return private(info, false, mode)
}

func openPrivate(dir *os.Root, name string, flag int, mode os.FileMode) (*os.File, error) {
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = private(before, false, mode); err != nil {
		return nil, err
	}
	f, err := dir.OpenFile(name, flag, mode)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, errors.New("session file changed")
	}
	return f, nil
}

func writeFile(dir *os.Root, name string, data []byte, mode os.FileMode) error {
	f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func copyExecutable(dir *os.Root, source string) error {
	// Resolve os.Executable paths on platforms where the installed binary is a
	// symlink. The source is trusted entrypoint input, not session metadata.
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxExecutable {
		return errors.New("session executable must be a regular file at most 64 MiB")
	}
	dst, err := dir.OpenFile("executor", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	size, err := io.Copy(dst, io.LimitReader(src, maxExecutable+1))
	if err == nil && size > maxExecutable {
		err = errors.New("session executable exceeds 64 MiB")
	}
	if err == nil {
		err = dst.Sync()
	}
	return errors.Join(err, dst.Close())
}

func readRecord(dir *os.Root, name string) (rec record, err error) {
	f, err := openPrivate(dir, name, os.O_RDONLY, 0600)
	if err != nil {
		return rec, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMetadata+1))
	if err != nil {
		return rec, err
	}
	if len(data) > maxMetadata {
		return rec, errRecordData
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, errRecordData
	}
	return rec, nil
}

// removeStaging validates the entire flat directory before removing anything.
// os.Root confines access, and Remove never traverses a child symlink.
func removeStaging(root *os.Root, token string, dir *os.Root) error {
	entries, err := entries(dir)
	if err != nil {
		return err
	}
	for _, name := range entries {
		mode := os.FileMode(0600)
		switch name {
		case "executor":
			mode = 0700
		case "lock", "record.tmp", "prepared.json", "claimed.json":
		default:
			return fmt.Errorf("unexpected session file %q", name)
		}
		if err = checkFile(dir, name, mode); err != nil {
			return err
		}
	}
	for _, name := range entries {
		if name != "lock" {
			if err = dir.Remove(name); err != nil {
				return err
			}
		}
	}
	if err = dir.Remove("lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return root.Remove(token)
}

func entries(dir *os.Root) ([]string, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(257)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > 256 {
		return nil, errors.New("too many session staging entries")
	}
	return names, nil
}

func (m *Manager) prune(root *os.Root) (int, error) {
	names, err := entries(root)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, token := range names {
		if token == ".lock" {
			continue
		}
		if err = validToken(token); err != nil {
			return 0, err
		}
		count++
		dir, e := openDir(root, token)
		if e != nil {
			return 0, e
		}
		e = m.pruneOne(root, token, dir)
		dir.Close()
		if e == nil {
			count--
		} else if !errors.Is(e, errRetain) {
			return 0, e
		}
	}
	return count, nil
}

var errRetain = errors.New("retain session")

func (m *Manager) pruneOne(root *os.Root, token string, dir *os.Root) error {
	lock, err := lockFile(dir, "lock", false)
	if errors.Is(err, errLocked) {
		return errRetain
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}
	info, err := dir.Stat(".")
	if err != nil {
		return err
	}
	expires := info.ModTime().Add(preparationTTL)
	for _, name := range []string{"prepared.json", "claimed.json"} {
		rec, e := readRecord(dir, name)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if errors.Is(e, errRecordData) {
			// An inactive, private but damaged record cannot block every new
			// session. Fall back to the directory's age; removeStaging still
			// validates all ownership, permissions and children before deletion.
			break
		}
		if e != nil {
			return e
		}
		expires = rec.Expires
		break
	}
	if m.Now().Before(expires) {
		return errRetain
	}
	return removeStaging(root, token, dir)
}
