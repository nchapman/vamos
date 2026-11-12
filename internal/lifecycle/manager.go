package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/process"
)

const caddyConfig = "{\n\tadmin 127.0.0.1:2019\n}\n"

// Applications will get explicit private networks; no shared default bridge.
const dockerConfig = "{\n  \"bridge\": \"none\",\n  \"log-driver\": \"local\"\n}\n"

var units = []string{"caddy.service", "docker.service", "docker.socket", "containerd.service"}
var dataPaths = []string{"/etc/caddy", "/etc/docker", "/etc/containerd", "/var/lib/caddy", "/var/lib/docker", "/var/lib/containerd", "/var/log/caddy", "/usr/local/lib/soar"}

type Runner func(context.Context, string, []string) (process.Result, error)

type Manager struct {
	// Root is only for isolated filesystem tests. Production always uses /.
	Root        string
	Run         Runner
	Inspect     func(context.Context) (*host.Report, error)
	Executor    []byte
	CaddyConfig func(context.Context) ([]byte, error)
	MountInfo   func() ([]byte, error)
}

func RunCommand(ctx context.Context, name string, args []string) (process.Result, error) {
	envArgs := append([]string{"LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", name}, args...)
	if name == "apt-get" && !contains(args, "--simulate") {
		return process.RunTail(ctx, "/usr/bin/env", envArgs, nil)
	}
	return process.Run(ctx, "/usr/bin/env", envArgs, nil)
}

func contains(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}
func (m *Manager) path(p string) string {
	return filepath.Join(m.Root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
}

func (m *Manager) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	r, err := m.Run(ctx, name, args)
	if err != nil {
		detail := append(append([]byte{}, r.Stdout...), r.Stderr...)
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		return nil, fmt.Errorf("%s failed: %w; output tail: %q", name, err, detail)
	}
	return r.Stdout, nil
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	report, err := m.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !report.Supported || !report.Root {
		return nil, errors.New("host lifecycle requires root on supported Ubuntu 24.04+ with systemd (amd64/arm64)")
	}
	if err := rejectSymlink(m.path(StatePath)); err != nil {
		return nil, err
	}
	if err := rejectSymlink(m.path(LockPath)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(m.path(LockPath)), 0755); err != nil {
		return nil, err
	}
	lock := flock.New(m.path(LockPath), flock.SetPermissions(0600))
	ok, err := lock.TryLock()
	if err != nil {
		lock.Close()
		return nil, err
	}
	if !ok {
		lock.Close()
		return nil, errors.New("another host lifecycle operation is running; inspect server status before retrying")
	}
	return func() { lock.Close() }, nil
}

func (m *Manager) save(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.path(StatePath), append(data, '\n'), 0600)
}

func (m *Manager) step(s *State, step string) error {
	s.Step = step
	s.LastError = ""
	return m.save(s)
}
func (m *Manager) recordError(s *State, err error) {
	if s != nil && err != nil {
		s.LastError = err.Error()
		_ = m.save(s)
	}
}

func (m *Manager) Bootstrap(ctx context.Context, config Config) (_ *State, err error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	if len(m.Executor) == 0 {
		return nil, errors.New("executor artifact is empty")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return nil, err
	}
	if _, err := m.appReceipts(s); err != nil {
		return nil, err
	}
	if s != nil && s.Phase != "removed" {
		if s.Config != config {
			return nil, errors.New("host belongs to a different server name/domain; refusing to change its identity")
		}
		if s.Phase == "removing" {
			return nil, errors.New("teardown is incomplete; finish teardown before bootstrapping again")
		}
		if s.Configured {
			if err := m.verifyFiles(s); err != nil {
				return nil, err
			}
		}
		if s.Phase == "ready" {
			installed, err := m.packages(ctx)
			if err != nil {
				return nil, err
			}
			if err := checkVersions(s, installed, true); err != nil {
				return nil, err
			}
			if err := m.health(ctx); err != nil {
				return nil, err
			}
			return s, nil
		}
	} else {
		if err := m.preflight(ctx, s != nil); err != nil {
			return nil, err
		}
		baseline, err := m.packages(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := m.command(ctx, "apt-get", "update", "-qq"); err != nil {
			return nil, err
		}
		plan, err := m.command(ctx, "apt-get", "--simulate", "--no-install-recommends", "--no-remove", "install", "docker.io", "docker-buildx", "caddy")
		if err != nil {
			return nil, err
		}
		packages, err := parseInstallPlan(plan, baseline)
		if err != nil {
			return nil, err
		}
		for _, required := range []string{"docker.io", "docker-buildx", "caddy"} {
			found := false
			for _, p := range packages {
				if p.Name == required {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("APT did not plan a fresh %s installation", required)
			}
		}
		s = &State{Version: 1, ID: newID(), Config: config, Phase: "installing", Step: "planned", Packages: packages}
		if err := m.save(s); err != nil {
			return nil, err
		}
	}
	defer func() { m.recordError(s, err) }()
	if err := m.step(s, "install-packages"); err != nil {
		return nil, err
	}
	if err := m.mask(ctx); err != nil {
		return nil, err
	}
	if err := m.installPackages(ctx, s); err != nil {
		return nil, err
	}
	if err := m.step(s, "configure"); err != nil {
		return nil, err
	}
	if !s.Configured {
		if err := atomicWrite(m.path("/etc/caddy/Caddyfile"), []byte(caddyConfig), 0644); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/docker/daemon.json"), []byte(dockerConfig), 0644); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path(ExecutorPath), m.Executor, 0755); err != nil {
			return nil, err
		}
		hash := sha256.Sum256(m.Executor)
		s.ExecutorSHA = hex.EncodeToString(hash[:])
		s.Accounts = map[string]string{}
		for _, key := range []string{"passwd:caddy", "group:caddy", "group:docker"} {
			parts := strings.Split(key, ":")
			value, err := m.account(ctx, parts[0], parts[1])
			if err != nil {
				return nil, err
			}
			s.Accounts[key] = value
		}
		if data, err := os.ReadFile(m.path("/etc/containerd/config.toml")); err == nil {
			hash := sha256.Sum256(data)
			s.ContainerdConfigSHA = hex.EncodeToString(hash[:])
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		s.Configured = true
		if err := m.save(s); err != nil {
			return nil, err
		}
	}
	if _, err := m.command(ctx, "caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return nil, err
	}
	if err := m.step(s, "start-services"); err != nil {
		return nil, err
	}
	if err := m.unmask(ctx); err != nil {
		return nil, err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", "containerd.service", "docker.socket", "docker.service", "caddy.service"); err != nil {
		return nil, err
	}
	if err := m.health(ctx); err != nil {
		return nil, err
	}
	s.Phase = "ready"
	if err := m.step(s, "complete"); err != nil {
		return nil, err
	}
	return s, nil
}

func (m *Manager) health(ctx context.Context) error {
	if _, err := m.command(ctx, "systemctl", "is-active", "docker.service", "caddy.service"); err != nil {
		return err
	}
	if _, err := m.command(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return err
	}
	if _, err := m.command(ctx, "docker", "buildx", "version"); err != nil {
		return err
	}
	if _, err := m.command(ctx, "caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	return m.checkCaddyRuntime(ctx)
}

func (m *Manager) mask(ctx context.Context) error {
	for _, unit := range units {
		path := m.path("/etc/systemd/system/" + unit)
		if err := rejectSymlink(filepath.Dir(path)); err != nil {
			return err
		}
		if dest, err := os.Readlink(path); err == nil && dest == "/dev/null" {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unexpected service override at %s", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.Symlink("/dev/null", path); err != nil {
			return err
		}
	}
	_, err := m.command(ctx, "systemctl", "daemon-reload")
	return err
}

func (m *Manager) unmask(ctx context.Context) error {
	for _, unit := range units {
		path := m.path("/etc/systemd/system/" + unit)
		dest, err := os.Readlink(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || dest != "/dev/null" {
			return fmt.Errorf("unexpected service override at %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	_, err := m.command(ctx, "systemctl", "daemon-reload")
	return err
}

func (m *Manager) verifyFiles(s *State) error {
	for path, expected := range map[string]string{"/etc/caddy/Caddyfile": caddyConfig, "/etc/docker/daemon.json": dockerConfig} {
		if err := rejectSymlink(m.path(path)); err != nil {
			return err
		}
		actual, err := os.ReadFile(m.path(path))
		if err != nil || string(actual) != expected {
			return fmt.Errorf("managed file %s is missing or changed; refusing to overwrite or remove it", path)
		}
	}
	if err := rejectSymlink(m.path(ExecutorPath)); err != nil {
		return err
	}
	data, err := os.ReadFile(m.path(ExecutorPath))
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != s.ExecutorSHA {
		return errors.New("installed executor digest changed; refusing removal or overwrite")
	}
	if err := rejectSymlink(m.path("/etc/containerd/config.toml")); err != nil {
		return err
	}
	data, err = os.ReadFile(m.path("/etc/containerd/config.toml"))
	if s.ContainerdConfigSHA == "" {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("unexpected containerd configuration")
		}
	} else {
		if err != nil {
			return err
		}
		hash = sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != s.ContainerdConfigSHA {
			return errors.New("containerd configuration changed; refusing teardown")
		}
	}
	for _, unit := range units {
		if err := m.checkOverrides(unit); err != nil {
			return err
		}
	}
	for dir, allowed := range map[string]string{"/etc/docker": "daemon.json", "/etc/caddy": "Caddyfile", "/etc/containerd": "config.toml"} {
		entries, err := os.ReadDir(m.path(dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() != allowed {
				return fmt.Errorf("unmanaged file in %s: %s", dir, e.Name())
			}
		}
	}
	return nil
}
