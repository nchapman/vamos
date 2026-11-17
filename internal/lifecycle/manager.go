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
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/process"
)

const (
	BaseCaddyfile = "{\n\tadmin 127.0.0.1:2019\n\torder coraza_waf first\n\torder cache before rewrite\n\tcache {\n\t\tdefault_cache_control no-store\n\t\tsimplefs {\n\t\t\tconfiguration {\n\t\t\t\tpath /data/cache\n\t\t\t\tsize 1000\n\t\t\t}\n\t\t}\n\t}\n}\n"
	caddyConfig   = BaseCaddyfile
	// Applications get explicit private networks; no shared default bridge.
	DockerDaemonPath   = "/etc/docker/daemon.json"
	DockerDaemonConfig = "{\n  \"bridge\": \"none\",\n  \"log-driver\": \"local\"\n}\n"
	dockerConfig       = DockerDaemonConfig
)

var deployKeyLine = regexp.MustCompile(`^(?:ssh-(?:ed25519|rsa)|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{68,}(?: [ -~]{0,511})?$`)

// ValidateDeployKey accepts exactly one OpenSSH public key line with a
// printable optional comment. Newlines, quoting, and options are rejected:
// the restricted forced-command prefix is authored by the lifecycle itself.
func ValidateDeployKey(line string) error {
	if len(line) > 4096 || strings.ContainsAny(line, "\n\r\t\"") {
		return errors.New("deploy key must be one printable public key line")
	}
	if !deployKeyLine.MatchString(line) {
		return errors.New("deploy key must be an ssh-ed25519, ssh-rsa, or ecdsa public key line")
	}
	return nil
}

// bridgeKeyLine renders the initial restricted bridge key for the default
// deployer principal, or empty when bootstrap runs without a deploy key.
func (m *Manager) bridgeKeyLine() []byte {
	if m.DeployKey == "" {
		return nil
	}
	line := "restrict,command=\"" + AgentPath + " internal-agent bridge --principal default-deployer\" " + m.DeployKey
	return []byte(line + "\n")
}

var units = []string{"docker.service", "docker.socket", "containerd.service"}

const agentUnit = "vamos-agent.service"

const agentServiceConfig = `[Unit]
Description=Vamos coordinator agent
After=docker.service

[Service]
Type=simple
ExecStart=/usr/local/lib/vamos/vamos-agent internal-agent
Restart=on-failure
RestartSec=2s
User=root
Group=root
UMask=0077
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`

const agentSSHConfig = `Match User vamos
    AuthorizedKeysFile /etc/vamos/authorized_keys
    AuthorizedKeysCommand none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PermitTTY no
    X11Forwarding no
    AllowTcpForwarding no
    AllowAgentForwarding no
    PermitTunnel no
    GatewayPorts no
    PermitUserRC no
`

const defaultPrincipals = `[
  {"id":"default-deployer","role":"deployer","apps":["*"]}
]
`

const defaultAgentConfig = `{
  "concurrency": {
    "max_queue_depth": 32,
    "max_staged_bytes": 536870912,
    "max_concurrent_builds": 1,
    "max_concurrent_deploys": 1
  }
}
`

var dataPaths = []string{"/etc/caddy", "/etc/docker", "/etc/containerd", "/var/lib/caddy", "/var/lib/docker", "/var/lib/containerd", "/usr/local/lib/vamos"}

type Runner func(context.Context, string, []string) (process.Result, error)

type Manager struct {
	// Root is only for isolated filesystem tests. Production always uses /.
	Root     string
	Run      Runner
	Inspect  func(context.Context) (*host.Report, error)
	Executor []byte
	// Agent is the installed coordinator. During bootstrap it may deliberately
	// match Executor: the bundle's internal-agent entry point dispatches it.
	Agent []byte
	// DeployKey is one validated SSH public key line installed as the initial
	// restricted bridge key. Empty leaves the ACL file absent for administrators
	// who provision keys out of band.
	DeployKey   string
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
		_ = lock.Close()
		return nil, err
	}
	if !ok {
		_ = lock.Close()
		return nil, errors.New("another host lifecycle operation is running; inspect server status before retrying")
	}
	return func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}, nil
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
	if len(m.Agent) == 0 {
		m.Agent = m.Executor
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
			if err := checkPackages(s, installed, true); err != nil {
				return nil, err
			}
			if err := m.health(ctx, s); err != nil {
				return nil, err
			}
			return s, nil
		}
	} else {
		if err := m.rejectRetainedVolumes(); err != nil {
			return nil, err
		}
		// Retained repository keys/configuration remain bound to the old host.
		backupPath := m.path("/var/lib/vamos/backups")
		if err := rejectSymlink(backupPath); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(backupPath); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("retained backup repository state prevents creating a new host identity; archive and reconcile its recovery material first")
		}
		// Retained secrets bind to the old host identity independently of app journals.
		secretPath := m.path("/var/lib/vamos/secrets")
		if err := rejectSymlink(secretPath); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(secretPath); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("retained secret state prevents creating a new host identity; explicitly remove retained secrets first")
		}
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
		plan, err := m.command(ctx, "apt-get", "--simulate", "--no-install-recommends", "--no-remove", "install", "docker.io", "docker-buildx")
		if err != nil {
			return nil, err
		}
		packages, err := parseInstallPlan(plan, baseline)
		if err != nil {
			return nil, err
		}
		for _, required := range []string{"docker.io", "docker-buildx"} {
			if !contains(packages, required) {
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
		if err := atomicWrite(m.path(caddyfilePath), []byte(caddyConfig), 0644); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path(DockerDaemonPath), []byte(dockerConfig), 0644); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path(ExecutorPath), m.Executor, 0755); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path(AgentPath), m.Agent, 0755); err != nil {
			return nil, err
		}
		// The restricted account's shell and forced command live here; sshd must
		// be able to traverse to them while the files stay root-owned.
		if err := os.MkdirAll(m.path("/usr/local/lib/vamos"), 0755); err != nil {
			return nil, err
		}
		if err := os.Chmod(m.path("/usr/local/lib/vamos"), 0755); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/vamos/agent.json"), []byte(defaultAgentConfig), 0600); err != nil {
			return nil, err
		}
		// sshd reads authorized_keys while checking the restricted account's
		// credentials, so the directory is traversable and the public key file
		// is world-readable. The private policy and principal files remain 0600.
		if err := os.MkdirAll(m.path("/etc/vamos"), 0755); err != nil {
			return nil, err
		}
		if err := os.Chmod(m.path("/etc/vamos"), 0755); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/vamos/principals.json"), []byte(defaultPrincipals), 0600); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/vamos/authorized_keys"), m.bridgeKeyLine(), 0644); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/ssh/sshd_config.d/vamos.conf"), []byte(agentSSHConfig), 0600); err != nil {
			return nil, err
		}
		if err := atomicWrite(m.path("/etc/systemd/system/"+agentUnit), []byte(agentServiceConfig), 0644); err != nil {
			return nil, err
		}
		if _, err := m.command(ctx, "useradd", "--system", "--user-group", "--no-create-home", "--home-dir", "/", "--shell", AgentPath, "vamos"); err != nil {
			return nil, err
		}
		if _, err := m.command(ctx, "sshd", "-t"); err != nil {
			return nil, fmt.Errorf("validate restricted vamos SSH configuration: %w", err)
		}
		// The running daemon must adopt the Match block before the restricted
		// account can authenticate; reload keeps existing sessions untouched.
		if _, err := m.command(ctx, "systemctl", "reload", "ssh"); err != nil {
			return nil, fmt.Errorf("reload ssh for the restricted vamos account: %w", err)
		}
		hash := sha256.Sum256(m.Executor)
		s.ExecutorSHA = hex.EncodeToString(hash[:])
		hash = sha256.Sum256(m.Agent)
		s.AgentSHA = hex.EncodeToString(hash[:])
		s.Accounts = map[string]string{}
		for _, key := range ownedAccounts {
			parts := strings.Split(key, ":")
			value, err := m.account(ctx, parts[0], parts[1])
			if err != nil {
				return nil, err
			}
			s.Accounts[key] = value
		}
		s.Configured = true
		if err := m.save(s); err != nil {
			return nil, err
		}
	}
	if err := m.step(s, "start-services"); err != nil {
		return nil, err
	}
	if err := m.unmask(ctx); err != nil {
		return nil, err
	}
	if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return nil, err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", "containerd.service", "docker.socket", "docker.service"); err != nil {
		return nil, err
	}
	if err := m.ensureCaddy(ctx, s); err != nil {
		return nil, err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", agentUnit); err != nil {
		return nil, err
	}
	if err := m.health(ctx, s); err != nil {
		return nil, err
	}
	s.Phase = "ready"
	if err := m.step(s, "complete"); err != nil {
		return nil, err
	}
	return s, nil
}

// Upgrade replaces the paired installed executor and coordinator, and moves the
// Caddy container to this release's pinned image, after proving every managed
// resource still matches the ownership receipt. It never adopts drift or
// retries an uncertain previous replacement silently.
func (m *Manager) Upgrade(ctx context.Context, config Config) (_ *State, err error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	if len(m.Executor) == 0 {
		return nil, errors.New("executor artifact is empty")
	}
	if len(m.Agent) == 0 {
		m.Agent = m.Executor
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
	if s == nil || s.Phase != "ready" || s.Config != config {
		return nil, errors.New("server is not a ready installation with the requested identity")
	}
	if err := m.verifyFilesFor(s, true); err != nil {
		return nil, err
	}
	if err := m.checkFirewall(ctx, s.ID); err != nil {
		return nil, err
	}
	if err := m.verifyQueueIdle(); err != nil {
		return nil, err
	}
	oldExecutor, err := os.ReadFile(m.path(ExecutorPath))
	if err != nil {
		return nil, err
	}
	oldAgent, err := os.ReadFile(m.path(AgentPath))
	if err != nil {
		return nil, err
	}
	oldExecutorSHA := s.ExecutorSHA
	oldAgentSHA := s.AgentSHA
	defer func() { m.recordError(s, err) }()
	agentStopped := false
	binariesUpdated := false
	defer func() {
		if err == nil {
			return
		}
		if binariesUpdated {
			_ = atomicWrite(m.path(ExecutorPath), oldExecutor, 0755)
			_ = atomicWrite(m.path(AgentPath), oldAgent, 0755)
			s.ExecutorSHA = oldExecutorSHA
			s.AgentSHA = oldAgentSHA
		}
		if !agentStopped {
			return
		}
		restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, startErr := m.command(restartCtx, "systemctl", "start", agentUnit); startErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to restart %s after aborted upgrade: %w", agentUnit, startErr))
		}
	}()
	if _, err = m.command(ctx, "systemctl", "stop", agentUnit); err != nil {
		return nil, err
	}
	agentStopped = true
	if err = m.verifyQueueIdle(); err != nil {
		return nil, err
	}
	// Routes and certificates live on the host, so replacing the container to
	// adopt a new pinned image keeps them; only in-flight requests are cut.
	if err = m.ensureCaddy(ctx, s); err != nil {
		return nil, err
	}
	binariesUpdated = true
	if err = atomicWrite(m.path(ExecutorPath), m.Executor, 0755); err != nil {
		return nil, err
	}
	if err = atomicWrite(m.path(AgentPath), m.Agent, 0755); err != nil {
		return nil, err
	}
	executorHash := sha256.Sum256(m.Executor)
	agentHash := sha256.Sum256(m.Agent)
	s.ExecutorSHA = hex.EncodeToString(executorHash[:])
	s.AgentSHA = hex.EncodeToString(agentHash[:])
	if err = m.save(s); err != nil {
		return nil, err
	}
	if _, err = m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return nil, err
	}
	if _, err = m.command(ctx, "systemctl", "start", agentUnit); err != nil {
		return nil, err
	}
	agentStopped = false
	if err = m.health(ctx, s); err != nil {
		agentStopped = true
		return nil, err
	}
	binariesUpdated = false
	if err = m.step(s, "complete"); err != nil {
		return nil, err
	}
	return s, nil
}

func (m *Manager) health(ctx context.Context, s *State) error {
	if _, err := m.command(ctx, "systemctl", "is-active", "docker.service", agentUnit); err != nil {
		return err
	}
	if _, err := m.command(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return err
	}
	if _, err := m.command(ctx, "docker", "buildx", "version"); err != nil {
		return err
	}
	if err := m.checkCaddy(ctx, s); err != nil {
		return err
	}
	name, args := CaddyCommand("validate", "--config", caddyfilePath, "--adapter", "caddyfile")
	if _, err := m.command(ctx, name, args...); err != nil {
		return err
	}
	if err := m.checkCaddyRuntime(ctx); err != nil {
		return err
	}
	return m.checkFirewall(ctx, s.ID)
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
	return m.verifyFilesFor(s, false)
}

func (m *Manager) verifyFilesFor(s *State, allowLiveRoutes bool) error {
	for path, expected := range map[string]string{DockerDaemonPath: dockerConfig, "/etc/ssh/sshd_config.d/vamos.conf": agentSSHConfig, "/etc/systemd/system/" + agentUnit: agentServiceConfig} {
		if err := rejectSymlink(m.path(path)); err != nil {
			return err
		}
		actual, err := os.ReadFile(m.path(path))
		if err != nil || string(actual) != expected {
			return fmt.Errorf("managed file %s is missing or changed; refusing to overwrite or remove it", path)
		}
	}
	caddyPath := m.path(caddyfilePath)
	if err := rejectSymlink(caddyPath); err != nil {
		return err
	}
	caddyInfo, err := os.Lstat(caddyPath)
	if err != nil || !caddyInfo.Mode().IsRegular() || caddyInfo.Size() > 1<<20 {
		return errors.New("managed file /etc/caddy/Caddyfile is missing or unsafe")
	}
	caddyBytes, err := os.ReadFile(caddyPath)
	if err != nil {
		return err
	}
	if allowLiveRoutes {
		if !strings.HasPrefix(string(caddyBytes), caddyConfig) {
			return errors.New("managed file /etc/caddy/Caddyfile is missing or changed; refusing upgrade")
		}
	} else if string(caddyBytes) != caddyConfig {
		return errors.New("managed file /etc/caddy/Caddyfile is missing or changed; refusing to overwrite or remove it")
	}
	// Principals and authorized keys are deliberately administrator-managed
	// dynamic ACL inputs. They must remain private regular files, but their
	// contents are not frozen into the lifecycle receipt.
	// Principals, policy, and the journal stay root-only. The authorized_keys
	// file holds only public data and must be readable by sshd's credential
	// check for the restricted account, so it is mode 0644.
	for _, path := range []string{"/etc/vamos/agent.json", "/etc/vamos/principals.json"} {
		info, err := os.Lstat(m.path(path))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
			return fmt.Errorf("managed ACL file %s is missing or unsafe", path)
		}
	}
	keyInfo, err := os.Lstat(m.path("/etc/vamos/authorized_keys"))
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode()&os.ModeSymlink != 0 || keyInfo.Mode().Perm() != 0644 {
		return errors.New("managed bridge key file /etc/vamos/authorized_keys is missing or unsafe")
	}
	for path, digest := range map[string]string{ExecutorPath: s.ExecutorSHA, AgentPath: s.AgentSHA} {
		if err := rejectSymlink(m.path(path)); err != nil {
			return err
		}
		data, err := os.ReadFile(m.path(path))
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != digest {
			return fmt.Errorf("installed %s digest changed; refusing removal or overwrite", filepath.Base(path))
		}
	}
	for _, unit := range units {
		if err := m.checkOverrides(unit); err != nil {
			return err
		}
	}
	// /etc/containerd belongs to the containerd package: Ubuntu updates may
	// refresh it, and purge removes it.
	for dir, allowed := range map[string]string{"/etc/docker": "daemon.json", "/etc/caddy": "Caddyfile", "/etc/vamos": "agent.json authorized_keys principals.json"} {
		entries, err := os.ReadDir(m.path(dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !contains(strings.Fields(allowed), e.Name()) {
				return fmt.Errorf("unmanaged file in %s: %s", dir, e.Name())
			}
		}
	}
	return nil
}
