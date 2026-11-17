package firewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/process"
)

const baseline = "*filter\n:INPUT ACCEPT [0:0]\n:FORWARD DROP [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n"

type harness struct {
	m            *Manager
	t            *testing.T
	calls        []string
	installed    bool
	drift        bool
	fail         string
	rollbackFail bool
}

func fixture(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("server firewall persistence requires POSIX directory fsync and permissions")
	}
	t.Setenv("SSH_CONNECTION", "203.0.113.4 50000 10.0.0.1 2222")
	h := &harness{t: t, m: &Manager{Root: t.TempDir()}}
	h.m.Run = h.run
	if err := h.m.atomic("/etc/os-release", []byte("ID=ubuntu\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *harness) run(_ context.Context, name string, args []string) (process.Result, error) {
	call := name + " " + strings.Join(args, " ")
	h.calls = append(h.calls, call)
	if h.fail != "" && strings.Contains(call, h.fail) {
		return process.Result{}, errors.New("injected failure")
	}
	out := ""
	switch name {
	case "systemctl":
		if len(args) > 1 && args[0] == "show" {
			if args[1] == "docker.service" {
				out = "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock ; }"
			} else {
				out = "LoadState=not-found\nActiveState=inactive\n"
			}
		}
	case "docker":
		id := strings.Repeat("a", 64)
		switch args[0] {
		case "info":
			out = `{"Swarm":{"LocalNodeState":"inactive"}}`
		case "network":
			if args[1] == "ls" {
				out = id
			} else {
				out = `[{"Id":"` + id + `","Name":"vamos-test","Labels":{"vamos.host":"host","vamos.app":"test"},"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.17.0.0/16"}]},"Options":{"com.docker.network.bridge.name":"br-aaaaaaaaaaaa"}}]`
			}
		}
	case "ip":
		if args[len(args)-1] == "show" && strings.Contains(call, "link") {
			out = `[{"ifname":"lo"},{"ifname":"eth0"},{"ifname":"br-aaaaaaaaaaaa","linkinfo":{"info_kind":"bridge"}}]`
		} else if strings.Contains(call, "rule") {
			out = `[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main"}]`
		} else {
			out = "[]"
		}
	case "iptables", "ip6tables":
		out = "iptables v1.8.10 (legacy)"
	case "iptables-save", "ip6tables-save":
		out = baseline
		if h.installed {
			out += "VAMOS-FW-IN\n"
		}
	case "iptables-nft-save", "ip6tables-nft-save":
		out = ""
	case "nft":
		out = ""
	case "ss":
		out = `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=1,fd=3))` + "\n" + `LISTEN 0 128 [::]:2200 [::]:* users:(("sshd",pid=1,fd=4))`
	case "sshd":
		out = "port 2222\nport 2200\n"
	case "/bin/sh":
		action := args[1]
		switch action {
		case "install":
			h.installed = true
		case "verify":
			if !h.installed || h.drift {
				return process.Result{}, errors.New("rules differ")
			}
		case "remove", "rollback":
			if action == "rollback" && h.rollbackFail {
				return process.Result{}, errors.New("rollback failed")
			}
			h.installed = false
			os.Remove(h.m.path(confirmedPath))
			if err := os.Rename(h.m.path(statePath), h.m.path(retiredPath)); err != nil {
				return process.Result{}, err
			}
		}
	default:
		h.t.Fatalf("unexpected command %s", call)
	}
	return process.Result{Stdout: []byte(out)}, nil
}

type systemctlExit int

func (e systemctlExit) Error() string { return "systemctl failed" }
func (e systemctlExit) ExitCode() int { return int(e) }

func TestDisableHandlesUnloadedUnitButPreservesStopErrors(t *testing.T) {
	for _, code := range []int{5, 1} {
		h := fixture(t)
		ctx := context.Background()
		if _, err := h.m.Enable(ctx, "host"); err != nil {
			t.Fatal(err)
		}
		original := h.m.Run
		h.m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
			if name == "systemctl" && strings.Join(args, " ") == "stop vamos-firewall.service" {
				return process.Result{}, systemctlExit(code)
			}
			return original(ctx, name, args)
		}
		_, err := h.m.Disable(ctx, "host")
		if (err == nil) != (code == 5) {
			t.Fatalf("exit=%d, err=%v", code, err)
		}
	}
}

func TestDisableDoesNotStopDockerDependency(t *testing.T) {
	h := fixture(t)
	ctx := context.Background()
	if _, err := h.m.Enable(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	original := h.m.Run
	detached := false
	h.m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "systemctl daemon-reload" {
			_, err := os.Stat(h.m.path(dropinPath))
			detached = os.IsNotExist(err)
		}
		if strings.Contains(call, "vamos-firewall.service") && (strings.Contains(call, "--now") || strings.Contains(call, " stop ")) && !detached {
			t.Fatal("stopping firewall would propagate to Docker")
		}
		return original(ctx, name, args)
	}
	if _, err := h.m.Disable(ctx, "host"); err != nil {
		t.Fatal(err)
	}
}

func TestStagedConfirmDisableReenable(t *testing.T) {
	h := fixture(t)
	ctx := context.Background()
	st, err := h.m.Enable(ctx, "host")
	if err != nil || !st.Enabled || !st.Pending || len(st.Token) != 64 {
		t.Fatalf("enable: %+v %v", st, err)
	}
	if len(st.SSHPorts) != 2 || st.SSHPorts[0] != 2200 || st.SSHPorts[1] != 2222 {
		t.Fatalf("actual SSH ports: %v", st.SSHPorts)
	}
	calls := strings.Join(h.calls, "\n")
	if strings.Index(calls, "is-active vamos-firewall-rollback.timer") > strings.Index(calls, " install --locked") {
		t.Fatal("rules before independent rollback")
	}
	if _, err = h.m.Confirm(ctx, "host", st.Token); err == nil {
		t.Fatal("same SSH connection confirmed")
	}
	t.Setenv("SSH_CONNECTION", "203.0.113.4 50001 10.0.0.1 2222")
	if _, err = h.m.Confirm(ctx, "host", "wrong"); err == nil {
		t.Fatal("wrong token confirmed")
	}
	confirmed, err := h.m.Confirm(ctx, "host", st.Token)
	if err != nil || !confirmed.Enabled || confirmed.Pending || confirmed.Token != "" {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	if _, err = h.m.Disable(ctx, "other-host"); err == nil {
		t.Fatal("ownership mismatch allowed")
	}
	disabled, err := h.m.Disable(ctx, "host")
	if err != nil || disabled.Enabled || h.installed {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
	again, err := h.m.Enable(ctx, "host")
	if err != nil || again.Token == st.Token {
		t.Fatalf("reenable: %+v %v", again, err)
	}
}
func TestEnableDoesNotResetTimer(t *testing.T) {
	h := fixture(t)
	st, err := h.m.Enable(context.Background(), "host")
	if err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	again, err := h.m.Enable(context.Background(), "host")
	if err != nil || again.Token != st.Token {
		t.Fatal(again, err)
	}
	if strings.Contains(strings.Join(h.calls, "\n"), "restart") {
		t.Fatal("pending rollback extended")
	}
}
func TestStagingFailureNeverChangesRules(t *testing.T) {
	h := fixture(t)
	h.fail = "restart vamos-firewall-rollback.timer"
	if _, err := h.m.Enable(context.Background(), "host"); err == nil {
		t.Fatal("accepted timer failure")
	}
	if h.installed || strings.Contains(strings.Join(h.calls, "\n"), " install --locked") {
		t.Fatal("rules changed before rollback staged")
	}
}
func TestRollbackFailureRetainsIdentity(t *testing.T) {
	h := fixture(t)
	h.fail = " install --locked"
	h.rollbackFail = true
	st, err := h.m.Enable(context.Background(), "host")
	if err == nil || !st.Degraded || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatal(st, err)
	}
	if _, err := os.Stat(h.m.path(statePath)); err != nil {
		t.Fatal("lost recovery identity", err)
	}
	if !strings.Contains(strings.Join(h.calls, "\n"), "is-active vamos-firewall-rollback.timer") {
		t.Fatal("no independent rollback")
	}
}
func TestRollbackReenable(t *testing.T) {
	h := fixture(t)
	ctx := context.Background()
	first, err := h.m.Enable(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.m.action(ctx, "rollback"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.Status(ctx, "host")
	if err != nil || st.Enabled {
		t.Fatal(st, err)
	}
	second, err := h.m.Enable(ctx, "host")
	if err != nil || !second.Pending || second.Token == first.Token {
		t.Fatal(second, err)
	}
}
func TestDriftBlocksDisable(t *testing.T) {
	for _, artifact := range []bool{false, true} {
		t.Run(map[bool]string{false: "rules", true: "artifact"}[artifact], func(t *testing.T) {
			h := fixture(t)
			ctx := context.Background()
			if _, err := h.m.Enable(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if artifact {
				if err := os.WriteFile(h.m.path(scriptPath), []byte("foreign"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				h.drift = true
			}
			h.calls = nil
			st, err := h.m.Disable(ctx, "host")
			if err == nil || !st.Degraded {
				t.Fatal(st, err)
			}
			if strings.Contains(strings.Join(h.calls, "\n"), " remove --locked") {
				t.Fatal("destructive operation on drift")
			}
		})
	}
}
func TestForeignArtifactAndInvalidState(t *testing.T) {
	h := fixture(t)
	if err := h.m.atomic(scriptPath, []byte("foreign"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Enable(context.Background(), "host"); err == nil {
		t.Fatal("overwrote artifact")
	}
	if err := h.m.atomic(statePath, []byte(`{"version":1,"host_id":"host","token":"';id"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Status(context.Background(), "host"); err == nil {
		t.Fatal("accepted generated-data injection")
	}
}
func TestScriptGeneration(t *testing.T) {
	s := state{Token: strings.Repeat("a", 64), Plan: plan{SSHPorts: []int{22, 2222}, Bridges: []string{"br-+", "vamos-build0"}}}
	body := script(s)
	file := filepath.Join(t.TempDir(), "firewall.sh")
	if err := os.WriteFile(file, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	// Keep generated-content checks on every client OS; only shell execution
	// depends on a POSIX host.
	if runtime.GOOS != "windows" {
		if out, err := exec.Command("/bin/sh", "-n", file).CombinedOutput(); err != nil {
			t.Fatalf("shell syntax: %s %v", out, err)
		}
	}
	for _, want := range []string{"flock -x 9", "if confirmed; then exit 0", "if ! confirmed; then remove_owned", "subset_v4_early\n    subset_v6_early\n    remove_v4", "sync -f /var/lib/vamos", "ip6tables -w 10 -I FORWARD", "identity"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing safety invariant %q", want)
		}
	}
	for _, bad := range []string{"iptables-restore", "nft flush", "-F INPUT", "-F FORWARD", "-w 10 -P FORWARD", "-F DOCKER"} {
		if strings.Contains(body, bad) {
			t.Errorf("foreign mutation %s", bad)
		}
	}
	if !strings.Contains(service, "Before=network-pre.target docker.service") || !strings.Contains(dropin, "ExecStartPre=") || !strings.Contains(rollbackService, "Restart=on-failure") {
		t.Fatal("boot/restart protection missing")
	}
}
func TestExpiredConfirmation(t *testing.T) {
	h := fixture(t)
	ctx := context.Background()
	st, err := h.m.Enable(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.m.path(statePath))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err = json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	s.Deadline = s.Deadline.Add(-10 * 60 * 1e9)
	if err = h.m.save(s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_CONNECTION", "203.0.113.4 50001 10.0.0.1 2222")
	if _, err = h.m.Confirm(ctx, "host", st.Token); err == nil {
		t.Fatal("expired confirmation accepted")
	}
}
func TestRootRequiresRunner(t *testing.T) {
	m := Manager{Root: t.TempDir()}
	if _, err := m.Enable(context.Background(), "host"); err == nil {
		t.Fatal("test root can affect real firewall")
	}
}

// Exercise the defensive !ok/nil branch even if a context implementation closes
// Done before reporting Err. No successful status may be returned without a lock.
type doneWithoutError struct{ context.Context }

func (doneWithoutError) Done() <-chan struct{} { c := make(chan struct{}); close(c); return c }
func TestLockFailureAndStatusNoDataArtifacts(t *testing.T) {
	h := fixture(t)
	if _, err := h.m.Status(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.m.path("/var/lib/vamos")); !os.IsNotExist(err) {
		t.Fatal("status created data directory", err)
	}
	lock := flock.New(h.m.path(lockPath))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	called := false
	_, err := h.m.locked(doneWithoutError{context.Background()}, "host", func() (Status, error) { called = true; return Status{}, nil })
	if err == nil || called {
		t.Fatal("lock acquisition failure reported success", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Status(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
}

// Simulate process death after each unlink, as well as command failures after
// all unlinks. The retired identity must survive until cleanup completes.
func TestRetiredCleanupResumes(t *testing.T) {
	paths := []string{dropinPath, servicePath, timerPath, rollbackPath, scriptPath}
	for stage := 0; stage < len(paths)+2; stage++ {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			h := fixture(t)
			ctx := context.Background()
			if _, err := h.m.Enable(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if err := h.m.action(ctx, "rollback"); err != nil {
				t.Fatal(err)
			}
			if stage < len(paths) {
				for _, p := range paths[:stage+1] {
					if err := os.Remove(h.m.path(p)); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				h.fail = "daemon-reload"
				if stage > len(paths) {
					h.fail = "stop vamos-firewall.service"
				}
				if _, err := h.m.Disable(ctx, "host"); err == nil {
					t.Fatal("expected interruption")
				}
				if _, err := os.Stat(h.m.path(retiredPath)); err != nil {
					t.Fatal(err)
				}
				h.fail = ""
			}
			original := h.m.Run
			h.m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
				if name == "systemctl" && len(args) > 1 && (args[0] == "disable" || args[0] == "stop") {
					if _, e := os.Stat(h.m.path("/etc/systemd/system/" + args[len(args)-1])); os.IsNotExist(e) {
						if args[0] == "disable" {
							return process.Result{}, systemctlExit(1)
						}
						return process.Result{}, systemctlExit(5)
					}
				}
				return original(ctx, name, args)
			}
			if _, err := h.m.Disable(ctx, "host"); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if _, err := os.Stat(h.m.path(retiredPath)); !os.IsNotExist(err) {
				t.Fatalf("identity remains: %v", err)
			}
		})
	}
}

func TestRetiredCleanupRefusesChangedSurvivor(t *testing.T) {
	h := fixture(t)
	ctx := context.Background()
	if _, err := h.m.Enable(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.action(ctx, "rollback"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.m.path(scriptPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.m.path(dropinPath), []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Disable(ctx, "host"); err == nil {
		t.Fatal("accepted drift")
	}
	if b, err := os.ReadFile(h.m.path(dropinPath)); err != nil || string(b) != "foreign" {
		t.Fatal("removed foreign artifact")
	}
}

func TestRetiredCleanupRefusesReferences(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling-symlink", "rules"} {
		t.Run(kind, func(t *testing.T) {
			h := fixture(t)
			ctx := context.Background()
			if _, err := h.m.Enable(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if err := h.m.action(ctx, "rollback"); err != nil {
				t.Fatal(err)
			}
			if kind == "rules" {
				h.installed = true
			} else {
				target := filepath.Join(t.TempDir(), "foreign")
				if kind == "symlink" {
					b, err := os.ReadFile(h.m.path(dropinPath))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, b, 0644); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(h.m.path(dropinPath)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, h.m.path(dropinPath)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.m.Disable(ctx, "host"); err == nil {
				t.Fatal("accepted foreign reference")
			}
			if _, err := os.Stat(h.m.path(retiredPath)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(h.m.path(servicePath)); err != nil {
				t.Fatal("deleted artifacts before validation", err)
			}
		})
	}
}
