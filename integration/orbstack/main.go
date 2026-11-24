// Command orbstack runs the Linux integration suite in a disposable OrbStack VM.
package main

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type executor interface {
	Run(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}
type commandExecutor struct{}

func (commandExecutor) Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "orbctl", args...)
	c.Env = controlEnvironment(os.Environ())
	c.Stdin = in
	c.Stdout = out
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("orbctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// ORBENV can opt ambient Mac credentials into guest commands. This runner only
// transfers its explicit source snapshot and generated ownership token.
func controlEnvironment(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if !strings.HasPrefix(value, "ORBENV=") {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

type machine struct {
	Record struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Config struct {
			Isolated bool `json:"isolated"`
			Forward  bool `json:"forward_ssh_agent"`
			Network  bool `json:"isolate_network"`
		} `json:"config"`
	} `json:"record"`
}
type manifest struct {
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
	Token     string `json:"owner_token"`
	Cleanup   string `json:"cleanup"`
	Artifacts string `json:"artifacts"`
	Error     string `json:"error,omitempty"`
}
type options struct {
	root, artifacts, name, token, arch, ubuntu string
	scenario                                   string
	keep                                       bool
}
type runner struct {
	exec           executor
	out, stderr    io.Writer
	cleanupTimeout time.Duration
	restartTimeout time.Duration
	verifyTimeout  time.Duration
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func (r runner) info(ctx context.Context, id string) (machine, error) {
	var b strings.Builder
	var m machine
	err := r.exec.Run(ctx, []string{"info", "-f", "json", id}, nil, io.MultiWriter(&b, r.out), r.stderr)
	if err == nil {
		err = json.Unmarshal([]byte(b.String()), &m)
	}
	return m, err
}
func runArgs(id string, cmd ...string) []string {
	return append([]string{"run", "-m", id, "-u", "root"}, cmd...)
}
func (r runner) run(ctx context.Context, o options) (result error) {
	if err := os.MkdirAll(o.artifacts, 0700); err != nil {
		return err
	}
	m := manifest{Name: o.name, Token: o.token, Cleanup: "not-created", Artifacts: o.artifacts}
	save := func() error {
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(o.artifacts, "manifest.json"), append(b, '\n'), 0600)
	}
	if err := save(); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Manifest: %s/manifest.json; interrupted/SIGKILL runs may need manual cleanup. Never delete by name without verifying ownership.\n", o.artifacts)
	owned := false
	defer func() {
		// Cleanup must not inherit a canceled suite context.
		timeout := r.cleanupTimeout
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
		cleanup, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if owned {
			fresh, err := r.info(cleanup, m.ID)
			if err != nil || fresh.Record.ID != m.ID || fresh.Record.Name != m.Name {
				m.Cleanup = "retained: identity could not be verified"
				result = errors.Join(result, fmt.Errorf("cleanup identity verification failed: %v", err))
			} else {
				f, err := os.OpenFile(filepath.Join(o.artifacts, "guest-artifacts.tar"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
				if err == nil {
					err = r.exec.Run(cleanup, runArgs(m.ID, "sh", "-c", "if [ -d /opt/vamos-test/.integration-artifacts ]; then tar -C /opt/vamos-test/.integration-artifacts -cf - .; else tar -cf - --files-from /dev/null; fi"), nil, f, r.stderr)
					err = errors.Join(err, f.Close())
				}
				if err != nil {
					result = errors.Join(result, fmt.Errorf("collect artifacts: %w", err))
				}
				if result != nil && o.keep {
					m.Cleanup = "retained (--keep-on-failure)"
				} else {
					// Recheck after collection, immediately before deletion.
					fresh, err = r.info(cleanup, m.ID)
					if err == nil && fresh.Record.ID == m.ID && fresh.Record.Name == m.Name {
						err = r.exec.Run(cleanup, []string{"delete", "--force", m.ID}, nil, r.out, r.stderr)
					} else {
						err = fmt.Errorf("refusing deletion: identity mismatch or unavailable: %v", err)
					}
					if err != nil {
						m.Cleanup = "delete failed"
						result = errors.Join(result, err)
					} else {
						m.Cleanup = "deleted"
					}
				}
			}
		}
		if result != nil {
			m.Error = result.Error()
		}
		result = errors.Join(result, save())
		fmt.Fprintf(r.out, "Cleanup: %s; artifacts: %s\n", m.Cleanup, m.Artifacts)
		if owned && m.Cleanup != "deleted" {
			fmt.Fprintf(r.out, "Manual cleanup of owned VM: orbctl delete --force %s\n", m.ID)
		}
	}()
	archive := filepath.Join(o.artifacts, "source.tar")
	if err := archiveSource(o.root, archive); err != nil {
		return err
	}
	// A successful list is required: an unavailable daemon is not proof of absence.
	var list strings.Builder
	if err := r.exec.Run(ctx, []string{"list", "-f", "json"}, nil, io.MultiWriter(&list, r.out), r.stderr); err != nil {
		return err
	}
	var existing any
	if err := json.Unmarshal([]byte(list.String()), &existing); err != nil {
		return fmt.Errorf("decode machine list: %w", err)
	}
	if containsName(existing, o.name) {
		return fmt.Errorf("name already exists: %s", o.name)
	}
	m.Cleanup = "creation pending; ownership not established"
	if err := save(); err != nil {
		return err
	}
	args := []string{"create", "--isolated", "--isolate-network", "--user", "vamostest", "--memory", "4G", "--cpus", "4", "--disk", "20G"}
	if o.arch != "native" {
		args = append(args, "-a", o.arch)
	}
	args = append(args, "ubuntu:"+o.ubuntu, o.name)
	if err := r.exec.Run(ctx, args, nil, r.out, r.stderr); err != nil {
		m.Cleanup = "ambiguous creation: retained; inspect manually"
		return err
	}
	vm, err := r.info(ctx, o.name)
	if err != nil {
		return err
	}
	if vm.Record.Name != o.name || vm.Record.ID == "" {
		return errors.New("created machine identity mismatch; retained")
	}
	m.ID = vm.Record.ID
	owned = true
	m.Cleanup = "owned"
	if err := save(); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Owned VM %s; emergency cleanup: orbctl delete --force %s\n", m.ID, m.ID)
	if !vm.Record.Config.Isolated || vm.Record.Config.Forward || !vm.Record.Config.Network {
		return errors.New("machine isolation/agent/network verification failed")
	}
	call := func(in io.Reader, cmd ...string) error {
		return r.exec.Run(ctx, runArgs(m.ID, cmd...), in, r.out, r.stderr)
	}
	if err := call(strings.NewReader(o.token+"\n"), "sh", "-c", "umask 077; mkdir -p /var/lib /opt/vamos-test && cat > /var/lib/vamos-integration-owner"); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	err = call(f, "tar", "-x", "-C", "/opt/vamos-test")
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Guest prepare phase")
	prepareArgs := []string{"python3", "/opt/vamos-test/integration/linux_suite.py", "--owner", o.token}
	switch o.scenario {
	case "host-network":
		prepareArgs[1] = "/opt/vamos-test/integration/orbstack/host_network.py"
	case "firewall", "waf-cache", "sudo", "caddy-patch", "jobs":
		prepareArgs = append(prepareArgs, "--scenario", o.scenario)
	}
	if err := call(nil, prepareArgs...); err != nil {
		return fmt.Errorf("guest prepare: %w", err)
	}
	if o.scenario == "host-network" || o.scenario == "caddy-patch" || o.scenario == "jobs" {
		return nil // Single-phase acceptance.
	}
	// Prepare exit zero certifies the private guest checkpoint is ready. Always
	// restart: the verification phase itself rejects a missing checkpoint.
	verifyOwned := func(phase context.Context, stage string) error {
		fresh, err := r.info(phase, m.ID)
		if err != nil {
			return fmt.Errorf("%s machine verification: %w", stage, err)
		}
		if fresh.Record.ID != m.ID || fresh.Record.Name != m.Name {
			return fmt.Errorf("%s machine identity mismatch", stage)
		}
		if !fresh.Record.Config.Isolated || fresh.Record.Config.Forward || !fresh.Record.Config.Network {
			return fmt.Errorf("%s machine isolation/agent/network verification failed", stage)
		}
		return nil
	}
	restartTimeout := r.restartTimeout
	if restartTimeout == 0 {
		restartTimeout = 3 * time.Minute
	}
	restartCtx, cancelRestart := context.WithTimeout(ctx, restartTimeout)
	defer cancelRestart()
	if err := verifyOwned(restartCtx, "pre-restart"); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Restarting owned VM %s\n", m.ID)
	if err := r.exec.Run(restartCtx, []string{"restart", m.ID}, nil, r.out, r.stderr); err != nil {
		return fmt.Errorf("restart owned machine: %w", err)
	}
	fmt.Fprintln(r.out, "Post-restart machine identity and isolation verification")
	if err := verifyOwned(restartCtx, "post-restart"); err != nil {
		return err
	}
	cancelRestart()
	// orbctl restart blocks; the guest waits event-driven for systemd readiness.
	verifyTimeout := r.verifyTimeout
	if verifyTimeout == 0 {
		verifyTimeout = 10 * time.Minute
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, verifyTimeout)
	defer cancelVerify()
	fmt.Fprintln(r.out, "Guest verify-restart phase")
	verifyArgs := []string{"python3", "/opt/vamos-test/integration/linux_suite.py", "--owner", o.token, "--phase", "verify-restart"}
	if o.scenario == "firewall" || o.scenario == "waf-cache" || o.scenario == "sudo" {
		verifyArgs = append(verifyArgs, "--scenario", o.scenario)
	}
	if err := r.exec.Run(verifyCtx, runArgs(m.ID, verifyArgs...), nil, r.out, r.stderr); err != nil {
		return fmt.Errorf("guest verify-restart: %w", err)
	}
	if o.scenario == "firewall" || o.scenario == "waf-cache" || o.scenario == "sudo" {
		return nil
	}
	// Run after all Docker ownership/teardown and reboot checks. Repository
	// records intentionally remain until deletion of this exact owned VM.
	if err := verifyOwned(ctx, "pre-repository"); err != nil {
		return err
	}
	repositoryCtx, cancelRepository := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelRepository()
	fmt.Fprintln(r.out, "Guest live S3 repository acceptance phase")
	if err := r.exec.Run(repositoryCtx, runArgs(m.ID, "python3", "/opt/vamos-test/integration/repository/acceptance.py", "--owner", o.token), nil, r.out, r.stderr); err != nil {
		return fmt.Errorf("guest repository acceptance: %w", err)
	}
	return nil
}
func containsName(v any, name string) bool {
	switch x := v.(type) {
	case string:
		return x == name
	case []any:
		for _, v := range x {
			if containsName(v, name) {
				return true
			}
		}
	case map[string]any:
		for k, v := range x {
			if k == name || containsName(v, name) {
				return true
			}
		}
	}
	return false
}
func archiveSource(root, dest string) (result error) {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	defer func() { result = errors.Join(result, tw.Close(), f.Close()) }()
	for _, base := range []string{"cmd", "internal", "integration", "examples", "docs/backup-plans.md", "docs/backups.md", "go.mod", "go.sum", "mise.toml"} {
		err = filepath.WalkDir(filepath.Join(root, base), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if name == ".git" || name == ".vamos" || name == ".pi" || name == "__pycache__" || path == filepath.Join(root, "integration", "visual", "node_modules") || strings.HasPrefix(name, ".env") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				return fmt.Errorf("refusing symlink/special file: %s", path)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = filepath.ToSlash(rel)
			if err = tw.WriteHeader(h); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			return errors.Join(err, src.Close())
		})
		if err != nil {
			return err
		}
	}
	return nil
}
func main() {
	if err := entry(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func entry() error {
	keep := flag.Bool("keep-on-failure", false, "retain owned VM on failure")
	arch := flag.String("arch", "native", "native, arm64, or amd64")
	ubuntu := flag.String("ubuntu", "24.04", "24.04 or 26.04")
	scenario := flag.String("scenario", "all", "all, firewall, waf-cache, caddy-patch, jobs, host-network, or sudo (focused acceptance checks)")
	flag.Parse()
	if (*scenario != "all" && *scenario != "firewall" && *scenario != "waf-cache" && *scenario != "sudo" && *scenario != "caddy-patch" && *scenario != "jobs" && *scenario != "host-network") || flag.NArg() != 0 || (*arch != "native" && *arch != "arm64" && *arch != "amd64") || (*ubuntu != "24.04" && *ubuntu != "26.04") {
		return errors.New("invalid arguments")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("OrbStack host runner requires macOS")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	name, err := randomHex(6)
	if err != nil {
		return err
	}
	token, err := randomHex(16)
	if err != nil {
		return err
	}
	name = "vamos-test-" + name
	dir := filepath.Join(root, ".pi", "integration", name)
	if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "runner.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r := runner{exec: commandExecutor{}, out: io.MultiWriter(os.Stdout, log), stderr: io.MultiWriter(os.Stderr, log)}
	return r.run(ctx, options{root: root, artifacts: dir, name: name, token: token, arch: *arch, ubuntu: *ubuntu, keep: *keep, scenario: *scenario})
}
