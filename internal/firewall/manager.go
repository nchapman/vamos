// Package firewall manages a dedicated Ubuntu host's iptables firewall. It is
// deliberately independent of host lifecycle state and requires explicit SSH
// confirmation after a staged enable. Root and Docker administrators are trusted.
package firewall

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/process"
)

type Runner func(context.Context, string, []string) (process.Result, error)
type Manager struct {
	// Root is filesystem injection for tests, not a chroot. Non-default Root
	// requires Run: subprocesses must never accidentally operate on the real host.
	Root string
	Run  Runner
}
type Status struct {
	Enabled  bool     `json:"enabled"`
	Pending  bool     `json:"pending"`
	Degraded bool     `json:"degraded"`
	Token    string   `json:"token,omitempty"`
	SSHPorts []int    `json:"ssh_ports,omitempty"`
	Problems []string `json:"problems,omitempty"`
}
type state struct {
	Version    int       `json:"version"`
	HostID     string    `json:"host_id"`
	Token      string    `json:"token"`
	Connection string    `json:"connection"`
	Deadline   time.Time `json:"deadline"`
	Plan       plan      `json:"plan"`
}

func (m *Manager) path(p string) string {
	root := m.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, strings.TrimPrefix(p, "/"))
}
func (m *Manager) command(ctx context.Context, name string, args ...string) (string, error) {
	run := m.Run
	if run == nil {
		if m.Root != "" && m.Root != "/" {
			return "", fmt.Errorf("test Root requires injected Run")
		}
		run = func(ctx context.Context, n string, a []string) (process.Result, error) {
			return process.Run(ctx, n, a, nil)
		}
	}
	r, err := run(ctx, name, args)
	// iptables-save can exit successfully while warning that nft expressions
	// could not be represented. Such incomplete inspection is not acceptable.
	if err == nil && len(r.Stderr) > 0 && (strings.HasSuffix(name, "-save") || name == "nft") {
		err = fmt.Errorf("unexpected diagnostic: %s", strings.TrimSpace(string(r.Stderr)))
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(r.Stdout), nil
}
func (m *Manager) locked(ctx context.Context, hostID string, fn func() (Status, error)) (Status, error) {
	if strings.TrimSpace(hostID) == "" || len(hostID) > 256 {
		return Status{}, fmt.Errorf("hostID must be nonempty and at most 256 bytes")
	}
	if m.Root != "" && m.Root != "/" && m.Run == nil {
		return Status{}, fmt.Errorf("test Root requires injected Run")
	}
	if err := os.MkdirAll(filepath.Dir(m.path(lockPath)), 0755); err != nil {
		return Status{}, err
	}
	l := flock.New(m.path(lockPath))
	defer l.Close()
	ok, err := l.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, fmt.Errorf("firewall lock not acquired: %w", lockFailure(ctx))
	}
	return fn()
}
func (m *Manager) load(hostID string) (*state, error) { return m.loadFile(hostID, statePath) }
func (m *Manager) loadFile(hostID, path string) (*state, error) {
	b, err := os.ReadFile(m.path(path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("invalid firewall state: %w", err)
	}
	tok, e := hex.DecodeString(s.Token)
	if s.Version != 1 || e != nil || len(tok) != 32 || s.HostID != hostID || s.Deadline.IsZero() {
		return nil, fmt.Errorf("firewall state ownership/schema mismatch")
	}
	if _, e = connectionPort(s.Connection); e != nil {
		return nil, e
	}
	if len(s.Plan.Bridges) == 0 || len(s.Plan.SSHPorts) == 0 {
		return nil, fmt.Errorf("invalid firewall plan")
	}
	seen := map[string]bool{}
	for _, b := range s.Plan.Bridges {
		if !bridgeName.MatchString(b) || seen[b] {
			return nil, fmt.Errorf("invalid bridge plan")
		}
		seen[b] = true
	}
	ports := map[int]bool{}
	for _, p := range s.Plan.SSHPorts {
		if p < 1 || p > 65535 || ports[p] {
			return nil, fmt.Errorf("invalid SSH port plan")
		}
		ports[p] = true
	}
	return &s, nil
}
func (m *Manager) save(s state) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return m.atomic(statePath, append(b, '\n'), 0600)
}
func (m *Manager) atomic(path string, data []byte, mode os.FileMode) error {
	path = m.path(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".firewall-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CheckOwnership validates the persisted identity and all owned artifacts.
// Lifecycle uses it to distinguish our Docker drop-in from foreign overrides.
func (m *Manager) CheckOwnership(hostID string) error {
	s, err := m.load(hostID)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("missing managed firewall identity")
	}
	return m.checkArtifacts(*s)
}

func matchesOwnedArtifact(s state, p, got, want string, allowLegacy bool) bool {
	return got == want || (allowLegacy && p == scriptPath && (got == legacyV030Script(s) || got == previousScript(s)))
}

func (m *Manager) checkArtifacts(s state) error {
	return m.checkArtifactsFor(s, false)
}

func (m *Manager) checkArtifactsFor(s state, allowLegacy bool) error {
	for p, want := range artifacts(s) {
		got, err := os.ReadFile(m.path(p))
		if err != nil {
			return err
		}
		if !matchesOwnedArtifact(s, p, string(got), want, allowLegacy) {
			if matchesOwnedArtifact(s, p, string(got), want, true) {
				return errors.New("managed firewall script does not match this release; run vamos server upgrade")
			}
			return fmt.Errorf("owned artifact drift: %s", p)
		}
	}
	return nil
}
func (m *Manager) action(ctx context.Context, action string) error {
	_, err := m.command(ctx, "/bin/sh", m.path(scriptPath), action, "--locked")
	return err
}
func (m *Manager) confirmed(s state) (bool, error) {
	b, err := os.ReadFile(m.path(confirmedPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(b) != s.Token+"\n" {
		return false, fmt.Errorf("confirmation identity mismatch")
	}
	return true, nil
}
func (m *Manager) status(ctx context.Context, s *state) (Status, error) {
	return m.statusFor(ctx, s, false)
}

func (m *Manager) statusFor(ctx context.Context, s *state, allowLegacy bool) (Status, error) {
	if s == nil {
		// An absent JSON file is not evidence that old/foreign owned-name rules are absent.
		for _, tool := range []string{"iptables-save", "ip6tables-save"} {
			out, err := m.command(ctx, tool)
			if err != nil {
				return Status{Degraded: true, Problems: []string{err.Error()}}, err
			}
			if strings.Contains(out, "VAMOS-FW-") {
				err = fmt.Errorf("orphaned owned-name rules")
				return Status{Degraded: true, Problems: []string{err.Error()}}, err
			}
		}
		return Status{}, nil
	}
	st := Status{Token: s.Token, SSHPorts: append([]int(nil), s.Plan.SSHPorts...)}
	yes, err := m.confirmed(*s)
	st.Pending = !yes
	if err == nil {
		err = m.checkArtifactsFor(*s, allowLegacy)
	}
	if err == nil {
		err = m.action(ctx, "verify")
	}
	if err == nil && allowLegacy {
		got, readErr := os.ReadFile(m.path(scriptPath))
		err = readErr
		if err == nil && string(got) != script(*s) {
			rules := plan.rules
			if string(got) == legacyV030Script(*s) {
				rules = plan.legacyV030Rules
			}
			var check strings.Builder
			for _, family := range []string{"iptables", "ip6tables"} {
				for _, table := range []string{"filter", "mangle"} {
					check.WriteString(verifyOrder(family, table, rules(s.Plan, family)))
				}
			}
			_, err = m.command(ctx, "/bin/sh", "-ec", check.String())
		}
	}
	if err == nil {
		_, err = m.command(ctx, "systemctl", "is-enabled", "vamos-firewall.service")
	}
	if err == nil && st.Pending {
		_, err = m.command(ctx, "systemctl", "is-active", "vamos-firewall-rollback.timer")
		if err == nil && !time.Now().Before(s.Deadline) {
			err = fmt.Errorf("confirmation deadline expired; awaiting independent rollback")
		}
	}
	if err != nil {
		st.Degraded = true
		st.Problems = []string{err.Error()}
		return st, err
	}
	st.Enabled = true
	if !st.Pending {
		st.Token = ""
	}
	return st, nil
}
func (m *Manager) Status(ctx context.Context, hostID string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil {
			return Status{Degraded: true, Problems: []string{err.Error()}}, err
		}
		return m.status(ctx, s)
	})
}
func (m *Manager) Enable(ctx context.Context, hostID string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.locked(ctx, hostID, func() (Status, error) {
		old, err := m.load(hostID)
		if err != nil {
			return Status{}, err
		}
		if old != nil {
			return m.status(ctx, old)
		}
		if err = m.cleanRetired(ctx, hostID); err != nil {
			return Status{Degraded: true}, err
		}
		// Leftovers from successful autonomous rollback are recognized by the exact
		// generated script/state signature, not a mere filename. They are cleaned by
		// Disable; fail closed here rather than overwrite an unknown installation.
		for _, p := range []string{scriptPath, servicePath, timerPath, rollbackPath, dropinPath, confirmedPath} {
			if _, e := os.Lstat(m.path(p)); e == nil {
				return Status{}, fmt.Errorf("existing firewall artifact %s: inspect/clean rolled-back installation before enabling", p)
			} else if !os.IsNotExist(e) {
				return Status{}, e
			}
		}
		p, err := m.inspect(ctx, hostID)
		if err != nil {
			return Status{}, err
		}
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return Status{}, err
		}
		s := state{Version: 1, HostID: hostID, Token: hex.EncodeToString(raw), Connection: os.Getenv("SSH_CONNECTION"), Deadline: time.Now().UTC().Add(5 * time.Minute), Plan: p}
		// State first: any interruption is recognizable, never silently overwritten.
		if err = m.save(s); err != nil {
			return Status{}, err
		}
		for path, body := range artifacts(s) {
			mode := os.FileMode(0644)
			if path == scriptPath {
				mode = 0700
			}
			if err = m.atomic(path, []byte(body), mode); err != nil {
				return Status{Degraded: true}, err
			}
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "vamos-firewall.service"}, {"enable", "vamos-firewall-rollback.timer"}, {"restart", "vamos-firewall-rollback.timer"}, {"is-active", "vamos-firewall-rollback.timer"}} {
			if _, err = m.command(ctx, "systemctl", args...); err != nil {
				recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				rollbackErr := m.action(recovery, "rollback")
				cancel()
				return Status{Degraded: true, Pending: true, Token: s.Token}, errors.Join(fmt.Errorf("rollback staging failed; no rules changed: %w", err), rollbackErr)
			}
		}
		if err = m.action(ctx, "install"); err != nil {
			// Best effort immediate rollback with a new bounded context; the independent
			// timer remains armed even if this process dies or rollback itself fails.
			recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rollbackErr := m.action(recovery, "rollback")
			return Status{Degraded: true, Pending: true, Token: s.Token, Problems: []string{err.Error()}}, errors.Join(fmt.Errorf("firewall install failed: %w", err), rollbackErr)
		}
		return m.status(ctx, &s)
	})
}
func (m *Manager) Confirm(ctx context.Context, hostID, token string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil {
			return Status{}, err
		}
		if s == nil {
			return Status{}, fmt.Errorf("no pending firewall")
		}
		if token != s.Token {
			return Status{}, fmt.Errorf("confirmation token mismatch")
		}
		conn := os.Getenv("SSH_CONNECTION")
		port, err := connectionPort(conn)
		if err != nil {
			return Status{}, err
		}
		if conn == s.Connection {
			return Status{}, fmt.Errorf("confirmation requires a fresh SSH connection (not a multiplexed channel)")
		}
		allowed := false
		for _, p := range s.Plan.SSHPorts {
			if port == p {
				allowed = true
			}
		}
		if !allowed {
			return Status{}, fmt.Errorf("confirmation used an unapproved SSH port")
		}
		st, err := m.status(ctx, s)
		if err != nil {
			return st, err
		}
		if !st.Pending {
			return st, nil
		}
		if err = m.atomic(confirmedPath, []byte(s.Token+"\n"), 0600); err != nil {
			return st, err
		}
		if _, err = m.command(ctx, "systemctl", "stop", "vamos-firewall-rollback.timer"); err != nil {
			return Status{Enabled: true, Degraded: true}, err
		}
		// Do not start the service while holding its shared lock. At next boot it
		// restores confirmed rules; Docker's ExecStartPre verifies on every restart.
		return m.status(ctx, s)
	})
}
func (m *Manager) Disable(ctx context.Context, hostID string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil {
			return Status{}, err
		}
		if s == nil {
			if err = m.cleanRetired(ctx, hostID); err != nil {
				return Status{Degraded: true}, err
			}
			return m.status(ctx, nil)
		}
		st, err := m.status(ctx, s)
		if err != nil {
			return st, fmt.Errorf("refusing destructive disable of drifted firewall: %w", err)
		}
		if err = m.action(ctx, "remove"); err != nil {
			return Status{Degraded: true}, err
		}
		if err = m.cleanRetired(ctx, hostID); err != nil {
			return Status{Degraded: true}, err
		}
		return m.status(ctx, nil)
	})
}

// A successful independent rollback retires (rather than discards) its identity,
// allowing a subsequent operation to safely remove exact owned artifacts.
func (m *Manager) cleanRetired(ctx context.Context, hostID string) error {
	s, err := m.loadFile(hostID, retiredPath)
	if err != nil || s == nil {
		return err
	}
	// Only a retained, validated retired identity permits absent artifacts.
	// Lstat distinguishes absence from a dangling/foreign symlink; never follow
	// references when deciding whether an artifact is ours to delete.
	for p, want := range artifacts(*s) {
		info, e := os.Lstat(m.path(p))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("owned artifact is not a regular file: %s", p)
		}
		got, e := os.ReadFile(m.path(p))
		if e != nil {
			return e
		}
		if !matchesOwnedArtifact(*s, p, string(got), want, true) {
			return fmt.Errorf("owned artifact drift: %s", p)
		}
	}
	if _, err = m.status(ctx, nil); err != nil {
		return err
	}
	for _, args := range [][]string{{"disable", "--now", "vamos-firewall-rollback.timer"}, {"stop", "vamos-firewall-rollback.service"}, {"disable", "vamos-firewall.service"}} {
		// These units are disabled/stopped before any artifact deletion. A
		// missing unit on retry means that step already completed; systemctl
		// disable would otherwise fail on the now-uninstalled unit.
		unitPath := "/etc/systemd/system/" + args[len(args)-1]
		if _, e := os.Lstat(m.path(unitPath)); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return e
		}
		if _, err = m.command(ctx, "systemctl", args...); err != nil {
			return err
		}
	}
	for p := range artifacts(*s) {
		if err = os.Remove(m.path(p)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err = m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// Drop Docker's Requires edge before stopping the oneshot firewall unit.
	// Stopping it earlier propagates the stop to Docker and its workloads.
	if _, err = m.command(ctx, "systemctl", "stop", "vamos-firewall.service"); err != nil {
		// A never-started oneshot is unloaded by daemon-reload after its unit
		// file is removed. systemctl reports that already-stopped case as 5.
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != 5 {
			return err
		}
	}
	return os.Remove(m.path(retiredPath))
}

func lockFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("lock attempt ended without acquiring lock")
}

// CheckUpgradeable verifies that a managed firewall is confirmed and matches
// either this release or a known migratable release before server upgrade.
func (m *Manager) CheckUpgradeable(ctx context.Context, hostID string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil {
			return Status{}, err
		}
		st, err := m.statusFor(ctx, s, true)
		if err != nil {
			return st, err
		}
		if st.Pending {
			return st, errors.New("managed firewall confirmation is pending")
		}
		return st, nil
	})
	return err
}

// Upgrade migrates a confirmed firewall script and its active rules from a
// previous release (such as v0.3.0) to this release's rules, returning the
// previous script content when a migration occurred so the caller can roll back.
func (m *Manager) Upgrade(ctx context.Context, hostID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var oldScript string
	_, err := m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil {
			return Status{}, err
		}
		st, err := m.statusFor(ctx, s, true)
		if err != nil {
			return st, err
		}
		if st.Pending {
			return st, errors.New("managed firewall confirmation is pending")
		}
		if s == nil {
			return st, nil
		}
		got, err := os.ReadFile(m.path(scriptPath))
		if err != nil {
			return Status{}, err
		}
		want := script(*s)
		if string(got) == want {
			return st, nil
		}
		oldScript = string(got)
		if err := m.atomic(scriptPath, []byte(want), 0700); err != nil {
			oldScript = ""
			return Status{Degraded: true}, err
		}
		if oldScript == legacyV030Script(*s) {
			err = m.action(ctx, "upgrade")
		}
		if err == nil {
			st, err = m.status(ctx, s)
		}
		if err != nil {
			recovery, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if restoreErr := m.restoreLocked(recovery, *s, oldScript); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to restore firewall script after aborted upgrade: %w", restoreErr))
			} else {
				oldScript = ""
			}
			return Status{Degraded: true}, err
		}
		return st, nil
	})
	return oldScript, err
}

// RestoreScript rolls back a script migration performed by Upgrade.
func (m *Manager) RestoreScript(ctx context.Context, hostID, oldScript string) error {
	if oldScript == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := m.locked(ctx, hostID, func() (Status, error) {
		s, err := m.load(hostID)
		if err != nil || s == nil {
			return Status{}, errors.Join(err, errors.New("missing or invalid firewall state"))
		}
		st, err := m.status(ctx, s)
		if err != nil || st.Pending {
			return st, errors.Join(err, errors.New("cannot restore unverified firewall"))
		}
		return st, m.restoreLocked(ctx, *s, oldScript)
	})
	return err
}

func (m *Manager) restoreLocked(ctx context.Context, s state, oldScript string) error {
	switch oldScript {
	case legacyV030Script(s):
		if err := m.action(ctx, "restore-legacy"); err != nil {
			return err
		}
	case previousScript(s): // Same rules; only the verifier changed.
	default:
		return errors.New("unknown rollback script")
	}
	if err := m.atomic(scriptPath, []byte(oldScript), 0700); err != nil {
		return err
	}
	_, err := m.statusFor(ctx, &s, true)
	return err
}
