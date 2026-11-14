package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/process"
)

var testConfig = Config{Name: "test", Domain: "apps.example.com"}
var testPackages = []Package{{"caddy", "2.6"}, {"containerd", "2.2"}, {"docker-buildx", "0.30"}, {"docker.io", "29.1"}, {"runc", "1.4"}}

type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

type fakeSystem struct {
	t                 *testing.T
	m                 *Manager
	installed         map[string]installedPackage
	accounts          map[string]string
	active            bool
	calls             []string
	failInstall       bool
	failStart         bool
	foreign           bool
	foreignContainerd bool
	cache             int
	unownedRemoval    bool
}

func newTestManager(t *testing.T) (*Manager, *fakeSystem) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host filesystem lifecycle uses Unix symlinks; Windows client tested separately")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, Executor: []byte("test-executor"), Inspect: func(context.Context) (*host.Report, error) { return &host.Report{Supported: true, Root: true}, nil }}
	f := &fakeSystem{t: t, m: m, installed: map[string]installedPackage{"base-files": {"installed", "1"}}, accounts: map[string]string{}}
	m.Run = f.run
	m.MountInfo = func() ([]byte, error) { return nil, nil }
	m.CaddyConfig = func(context.Context) ([]byte, error) { return []byte(`{"admin":{"listen":"127.0.0.1:2019"}}`), nil }
	return m, f
}

func (f *fakeSystem) write(path, data string) {
	f.t.Helper()
	path = f.m.path(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fakeSystem) run(_ context.Context, name string, args []string) (process.Result, error) {
	command := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, command)
	out := func(s string) (process.Result, error) { return process.Result{Stdout: []byte(s)}, nil }
	switch name {
	case "dpkg-query":
		var names []string
		for n := range f.installed {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			p := f.installed[n]
			fmt.Fprintf(&b, "%s\t%s\t%s\n", n, p.Status, p.Version)
		}
		return out(b.String())
	case "dpkg", "ss":
		return out("")
	case "getent":
		value := f.accounts[args[0]+":"+args[1]]
		if value == "" {
			return process.Result{}, fakeExit(2)
		}
		return out(value + "\n")
	case "useradd":
		f.accounts["passwd:soar"] = "soar:x:997:997::/nonexistent:" + AgentPath
		f.accounts["group:soar"] = "soar:x:997:"
		return out("")
	case "systemctl":
		switch args[0] {
		case "show":
			if len(f.installed) > 1 {
				return out("loaded\n")
			}
			return out("not-found\n")
		case "daemon-reload":
			return out("")
		case "enable":
			if f.failStart {
				f.failStart = false
				return process.Result{}, fakeExit(1)
			}
			f.active = true
			f.write("/var/lib/docker/engine-state", "owned runtime data")
			return out("")
		case "stop":
			f.active = false
			return out("")
		case "restart":
			f.active = true
			return out("")
		case "is-active":
			if f.active {
				return out("active\n")
			}
			return process.Result{Stdout: []byte("inactive\n")}, fakeExit(3)
		}
	case "apt-get":
		if contains(args, "update") {
			return out("")
		}
		if contains(args, "install") {
			if contains(args, "--simulate") {
				var b strings.Builder
				for _, p := range testPackages {
					if _, ok := f.installed[p.Name]; !ok {
						fmt.Fprintf(&b, "Inst %s (%s Ubuntu:26.04 [amd64])\n", p.Name, p.Version)
					}
				}
				return out(b.String())
			}
			for _, unit := range units {
				dest, err := os.Readlink(f.m.path("/etc/systemd/system/" + unit))
				if err != nil || dest != "/dev/null" {
					f.t.Fatal("APT invoked without service masks")
				}
			}
			if f.failInstall {
				f.failInstall = false
				f.installed["caddy"] = installedPackage{"installed", "2.6"}
				f.accounts["passwd:caddy"] = "caddy:x:999:999:Caddy:/var/lib/caddy:/usr/sbin/nologin"
				f.accounts["group:caddy"] = "caddy:x:999:"
				f.write("/etc/caddy/Caddyfile", "package default")
				return process.Result{Stderr: []byte("simulated APT failure")}, fakeExit(1)
			}
			for _, p := range testPackages {
				f.installed[p.Name] = installedPackage{"installed", p.Version}
			}
			f.accounts["passwd:caddy"] = "caddy:x:999:999:Caddy:/var/lib/caddy:/usr/sbin/nologin"
			f.accounts["group:caddy"] = "caddy:x:999:"
			f.accounts["group:docker"] = "docker:x:998:"
			f.write("/etc/caddy/Caddyfile", "package default")
			f.write("/etc/containerd/config.toml", "containerd default")
			return out("")
		}
		if contains(args, "purge") {
			if contains(args, "--simulate") {
				var b strings.Builder
				for _, p := range testPackages {
					if _, ok := f.installed[p.Name]; ok {
						fmt.Fprintf(&b, "Purg %s [%s]\n", p.Name, p.Version)
					}
				}
				if f.unownedRemoval {
					b.WriteString("Remv unrelated [1]\n")
				}
				return out(b.String())
			}
			for _, p := range testPackages {
				delete(f.installed, p.Name)
			}
			return out("")
		}
	case "sshd":
		return out("")
	case "ctr":
		if f.foreignContainerd {
			return out("unowned-namespace\n")
		}
		return out("")
	case "caddy":
		if args[0] == "adapt" {
			return out(`{"admin":{"listen":"127.0.0.1:2019"}}`)
		}
		return out("Valid configuration\n")
	case "docker":
		if !f.active {
			return process.Result{}, fakeExit(1)
		}
		switch args[0] {
		case "info":
			return out("29.1\n")
		case "buildx":
			return out("v0.30\n")
		case "container":
			if f.foreign {
				return out("foreign-container\n")
			}
			return out("")
		case "image", "volume":
			return out("")
		case "network":
			return out("host\nnone\n")
		case "system":
			return out(fmt.Sprintf("{\"Type\":\"Build Cache\",\"TotalCount\":\"%d\"}\n", f.cache))
		}
	case "userdel":
		delete(f.accounts, "passwd:"+args[0])
		delete(f.accounts, "group:"+args[0])
		return out("")
	case "groupdel":
		delete(f.accounts, "group:"+args[0])
		return out("")
	}
	f.t.Fatalf("unexpected external command %q", command)
	return process.Result{}, errors.New("unexpected command")
}

func TestBootstrapRepeatTeardownRebootstrap(t *testing.T) {
	m, f := newTestManager(t)
	ctx := context.Background()
	first, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if first.Phase != "ready" || !f.active {
		t.Fatalf("state=%+v", first)
	}
	before := len(f.calls)
	second, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("repeated setup replaced installation identity")
	}
	for _, call := range f.calls[before:] {
		if strings.HasPrefix(call, "apt-get ") || strings.Contains(call, "systemctl enable") {
			t.Fatalf("repeat setup mutated healthy installation: %s", call)
		}
	}
	removed, err := m.Teardown(ctx, testConfig.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Phase != "removed" || f.active || len(f.installed) != 1 {
		t.Fatalf("state=%+v packages=%+v", removed, f.installed)
	}
	for _, path := range dataPaths {
		if _, err := os.Lstat(m.path(path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("owned path remains: %s", path)
		}
	}
	if _, err := os.Stat(m.path(StatePath)); err != nil {
		t.Fatal("receipt lost", err)
	}
	if _, err := os.Stat(m.path(LockPath)); err != nil {
		t.Fatal("stable lock removed", err)
	}
	if _, err := m.Teardown(ctx, testConfig.Name, true); err != nil {
		t.Fatal("repeat teardown", err)
	}
	third, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal("rebootstrap", err)
	}
	if third.Phase != "ready" || third.ID == first.ID {
		t.Fatalf("rebootstrap state=%+v", third)
	}
}

func TestInterruptedBootstrapCanResumeOrTeardown(t *testing.T) {
	for _, resume := range []bool{true, false} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			m, f := newTestManager(t)
			f.failInstall = true
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
				t.Fatal("wanted APT failure")
			}
			s, err := ReadState(m.path(StatePath))
			if err != nil || s == nil || s.Phase != "installing" || s.LastError == "" {
				t.Fatalf("missing durable failed operation: %+v %v", s, err)
			}
			if resume {
				_, err = m.Bootstrap(context.Background(), testConfig)
			} else {
				_, err = m.Teardown(context.Background(), testConfig.Name, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !resume {
				if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
					t.Fatalf("rebootstrap after partial teardown: %v", err)
				}
			}
		})
	}
}

func TestInterruptedStartResumesWithoutOverwritingConfig(t *testing.T) {
	m, f := newTestManager(t)
	f.failStart = true
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
		t.Fatal("wanted start failure")
	}
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownRefusesUnsafeConditions(t *testing.T) {
	for _, kind := range []string{"workload", "containerd", "cache", "config", "runtime-route", "mount", "package-version", "unowned-removal", "app-data", "account", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "workload":
				f.foreign = true
			case "containerd":
				f.foreignContainerd = true
			case "cache":
				f.cache = 1
			case "config":
				f.write("/etc/caddy/Caddyfile", "user changed it")
			case "runtime-route":
				m.CaddyConfig = func(context.Context) ([]byte, error) { return []byte(`{"apps":{"http":{}}}`), nil }
			case "mount":
				m.MountInfo = func() ([]byte, error) { return []byte("1 2 0:3 / /var/lib/docker/external rw - tmpfs tmpfs rw\n"), nil }
			case "package-version":
				f.installed["docker.io"] = installedPackage{"installed", "new-version"}
			case "unowned-removal":
				f.unownedRemoval = true
			case "app-data":
				f.write("/var/lib/soar/apps/important", "data")
			case "account":
				f.accounts["group:docker"] = "docker:x:998:other-user"
			case "symlink":
				os.RemoveAll(m.path("/var/lib/docker"))
				if err := os.Symlink(t.TempDir(), m.path("/var/lib/docker")); err != nil {
					t.Fatal(err)
				}
			}
			before := len(f.calls)
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
				t.Fatal("unsafe teardown succeeded")
			}
			if !f.active {
				t.Fatal("refusal stopped existing services")
			}
			for _, call := range f.calls[before:] {
				if strings.Contains(call, "apt-get -y") || strings.Contains(call, "systemctl stop") {
					t.Fatalf("refusal mutated host: %s", call)
				}
			}
		})
	}
}

func TestFreshSetupRefusesPreexistingResources(t *testing.T) {
	for _, kind := range []string{"data", "binary", "package", "state-dir", "account", "unit-mask"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			switch kind {
			case "data":
				f.write("/var/lib/docker/existing", "important")
			case "binary":
				f.write("/usr/local/bin/docker", "existing")
			case "package":
				f.installed["containerd"] = installedPackage{"installed", "old"}
			case "state-dir":
				f.write("/var/lib/soar/unmanaged", "important")
			case "account":
				f.accounts["group:docker"] = "docker:x:999:someone"
			case "unit-mask":
				path := m.path("/etc/systemd/system/docker.service")
				os.MkdirAll(filepath.Dir(path), 0755)
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
				t.Fatal("adopted unmanaged resource")
			}
			for _, call := range f.calls {
				if strings.HasPrefix(call, "apt-get ") {
					t.Fatalf("preflight refusal invoked APT: %s", call)
				}
			}
		})
	}
}

func TestPreflightRefusesFailedUnitLoadQueries(t *testing.T) {
	for _, output := range []string{"", "not-found\n"} {
		t.Run(fmt.Sprintf("output=%q", output), func(t *testing.T) {
			m, f := newTestManager(t)
			m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
				if name == "systemctl" && args[0] == "show" {
					return process.Result{Stdout: []byte(output)}, errors.New("systemd query failed")
				}
				return f.run(ctx, name, args)
			}
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
				t.Fatal("bootstrap ignored failed unit query")
			}
			if len(f.installed) != 1 {
				t.Fatal("query failure installed packages")
			}
		})
	}
}

func TestExplicitAuthorizationAndOwnershipRequired(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Teardown(context.Background(), "test", false); err == nil {
		t.Fatal("implicit purge allowed")
	}
	if _, err := m.Teardown(context.Background(), "test", true); err == nil {
		t.Fatal("cleanup without ownership allowed")
	}
	m.Inspect = func(context.Context) (*host.Report, error) { return &host.Report{Supported: true, Root: false}, nil }
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
		t.Fatal("non-root setup allowed")
	}
}

func TestTeardownRefusesFailedServiceQueries(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			m, f := newTestManager(t)
			s, err := m.Bootstrap(context.Background(), testConfig)
			if err != nil {
				t.Fatal(err)
			}
			if resume {
				s.Phase = "removing"
				s.Step = "stop-services"
				s.PurgeApproved = true
				if err := m.save(s); err != nil {
					t.Fatal(err)
				}
			}
			failed := false
			m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
				if !failed && name == "systemctl" && args[0] == "is-active" {
					failed = true
					return process.Result{}, errors.New("systemd bus unavailable")
				}
				return f.run(ctx, name, args)
			}
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
				t.Fatal("teardown proceeded after failed service query")
			}
			if !f.active || len(f.installed) == 1 {
				t.Fatal("query failure mutated host")
			}
		})
	}
}

func TestMutationLockRejectsConcurrentOperation(t *testing.T) {
	m, _ := newTestManager(t)
	os.MkdirAll(filepath.Dir(m.path(LockPath)), 0755)
	lock := flock.New(m.path(LockPath))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("got %v", err)
	}
}
