package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/process"
)

var testConfig = Config{Name: "test", Domain: "apps.example.com"}
var testPackages = []string{"containerd", "docker-buildx", "docker.io", "runc"}

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
	archiveVersion    string
	caddy             *fakeCaddy
	tagDigest         string
	release           int
	previous          *fakeCaddy
	caddyImages       map[string]string
	created           int
	agentStopped      bool
	foreignCtr        string
}

// fakeCaddy models only the inspect fields Vamos reads from the container.
type fakeCaddy struct {
	ImageID string
	ID      string
	Image   string
	Labels  map[string]string
	Running bool
	Args    []string
	Mutate  func(map[string]any)
}

func (c *fakeCaddy) inspect() string {
	doc := map[string]any{
		"Id": c.ID, "Image": c.ImageID,
		"Config": map[string]any{
			"Image":    c.Image,
			"User":     "0:0",
			"Cmd":      []string{"caddy", "run", "--config", caddyfilePath, "--adapter", "caddyfile"},
			"Labels":   map[string]string{caddyHostLabel: c.Labels[caddyHostLabel]},
			"Hostname": "ignored",
		},
		"State": map[string]any{"Running": c.Running, "Status": "ignored"},
		"HostConfig": map[string]any{
			"NetworkMode":    "host",
			"RestartPolicy":  map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0},
			"LogConfig":      map[string]any{"Type": "local", "Config": map[string]any{}},
			"Privileged":     false,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"CAP_NET_BIND_SERVICE"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Tmpfs":          map[string]string{"/tmp": ""},
			"Mounts": []map[string]any{
				{"Type": "bind", "Source": "/etc/caddy", "Target": "/etc/caddy", "ReadOnly": true},
				{"Type": "bind", "Source": caddyDataDir, "Target": "/data"},
				{"Type": "bind", "Source": caddyConfigDir, "Target": "/config"},
			},
		},
	}
	if c.Mutate != nil {
		c.Mutate(doc)
	}
	raw, _ := json.Marshal(doc)
	return string(raw)
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
	f := &fakeSystem{t: t, m: m, installed: map[string]installedPackage{"base-files": {"installed", "1"}}, accounts: map[string]string{}, caddyImages: map[string]string{}}
	m.Run = f.run
	m.MountInfo = func() ([]byte, error) { return nil, nil }
	m.CaddyConfig = func(context.Context) ([]byte, error) { return []byte(`{"admin":{"listen":"127.0.0.1:2019"}}`), nil }
	return m, f
}

func slicesIndex(values []string, wanted string) int {
	for i, v := range values {
		if v == wanted {
			return i
		}
	}
	return -1
}

func (f *fakeSystem) version() string {
	if f.archiveVersion != "" {
		return f.archiveVersion
	}
	return "1.0"
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
		f.accounts["passwd:vamos"] = "vamos:x:997:997::/:" + AgentPath
		f.accounts["group:vamos"] = "vamos:x:997:"
		return out("")
	case "systemctl":
		switch args[0] {
		case "is-enabled":
			return out("enabled\n")
		case "show":
			if unit := args[len(args)-1]; refreshUnits[unit] != "" {
				switch args[1] {
				case "--property=FragmentPath":
					return out(systemUnitDir + unit + "\n")
				case "--property=DropInPaths":
					return out("\n")
				}
				if _, err := os.Stat(f.m.path(systemUnitDir + args[len(args)-1])); os.IsNotExist(err) {
					return out("not-found\n")
				}
			}
			if len(f.installed) > 1 {
				return out("loaded\n")
			}
			return out("not-found\n")
		case "daemon-reload":
			return out("")
		case "reload":
			return out("")
		case "enable":
			if contains(args, refreshTimer) {
				p := f.m.path(refreshEnableLink)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					return process.Result{}, err
				}
				if err := os.Symlink(systemUnitDir+refreshTimer, p); err != nil && !os.IsExist(err) {
					return process.Result{}, err
				}
				return out("")
			}
			if f.failStart {
				f.failStart = false
				return process.Result{}, fakeExit(1)
			}
			f.active = true
			if contains(args, agentUnit) {
				f.agentStopped = false
			}
			f.write("/var/lib/docker/engine-state", "owned runtime data")
			return out("")
		case "stop":
			if args[1] == refreshTimer {
				return out("")
			}
			if args[1] == agentUnit {
				f.agentStopped = true
			} else {
				f.active = false
			}
			return out("")
		case "start", "restart":
			if args[1] == agentUnit {
				f.agentStopped = false
			} else {
				f.active = true
			}
			return out("")
		case "is-active":
			if f.active && !(f.agentStopped && contains(args, agentUnit)) {
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
					if _, ok := f.installed[p]; !ok {
						fmt.Fprintf(&b, "Inst %s (%s Ubuntu:26.04 [amd64])\n", p, f.version())
					}
				}
				return out(b.String())
			}
			for _, arg := range args {
				if name, version, pinned := strings.Cut(arg, "="); pinned && contains(testPackages, name) && version != f.version() {
					return process.Result{Stderr: []byte("version not found")}, fakeExit(100)
				}
			}
			for _, unit := range units {
				dest, err := os.Readlink(f.m.path("/etc/systemd/system/" + unit))
				if err != nil || dest != "/dev/null" {
					f.t.Fatal("APT invoked without service masks")
				}
			}
			if f.failInstall {
				f.failInstall = false
				f.installed["containerd"] = installedPackage{"installed", f.version()}
				f.write("/etc/containerd/config.toml", "containerd default")
				return process.Result{Stderr: []byte("simulated APT failure")}, fakeExit(1)
			}
			for _, p := range testPackages {
				f.installed[p] = installedPackage{"installed", f.version()}
			}
			f.accounts["group:docker"] = "docker:x:998:"
			f.write("/etc/containerd/config.toml", "containerd default")
			return out("")
		}
		if contains(args, "purge") {
			if contains(args, "--simulate") {
				var b strings.Builder
				for _, p := range testPackages {
					if installed, ok := f.installed[p]; ok {
						fmt.Fprintf(&b, "Purg %s [%s]\n", p, installed.Version)
					}
				}
				if f.unownedRemoval {
					b.WriteString("Remv unrelated [1]\n")
				}
				return out(b.String())
			}
			for _, p := range testPackages {
				delete(f.installed, p)
			}
			return out("")
		}
	case "sshd":
		return out("")
	case "ctr":
		if args[0] == "namespaces" {
			if f.foreignContainerd {
				return out("moby\nunowned-namespace\n")
			}
			return out("moby\n")
		}
		var items []string
		switch args[2] {
		case "containers":
			if f.caddy != nil {
				items = append(items, f.caddy.ID)
			}
		case "images":
			for ref := range f.caddyImages {
				if !strings.Contains(ref, "/") {
					ref = "docker.io/library/" + ref
				}
				items = append(items, ref)
			}
		case "content":
			if len(f.caddyImages) != 0 {
				items = append(items, "sha256:"+strings.Repeat("e", 64))
			}
		}
		if f.foreignCtr == args[2] {
			items = append(items, "foreign")
		}
		sort.Strings(items)
		return out(strings.Join(items, "\n"))
	case "docker":
		if !f.active {
			return process.Result{}, fakeExit(1)
		}
		switch args[0] {
		case "info":
			return out("29.1\n")
		case "buildx":
			return out("v0.30\n")
		case "pull":
			ref := args[len(args)-1]
			digest := fmt.Sprintf("sha256:%064x", f.release+100)
			id := fmt.Sprintf("sha256:%064x", f.release+200)
			if ref == CaddyImage {
				f.tagDigest = digest
				f.caddyImages[CaddyImage] = id
			} else {
				digest = caddyDigest
			}
			f.caddyImages[caddyRepo+"@"+digest] = id
			return out("")
		case "run":
			image := args[len(args)-7]
			if f.caddyImages[digestRef(image)] == "" {
				return process.Result{Stderr: []byte("image not pulled")}, fakeExit(125)
			}
			f.created++
			label, _ := strings.CutPrefix(args[slicesIndex(args, "--label")+1], caddyHostLabel+"=")
			f.caddy = &fakeCaddy{ID: fmt.Sprintf("%064x", f.created), Image: image, ImageID: f.caddyImages[digestRef(image)], Labels: map[string]string{caddyHostLabel: label}, Running: true, Args: args}
			return out(f.caddy.ID + "\n")
		case "exec":
			if f.caddy == nil || !f.caddy.Running || args[1] != CaddyContainer || args[2] != "caddy" {
				return process.Result{Stderr: []byte("container not running")}, fakeExit(1)
			}
			if args[3] == "adapt" {
				return out(`{"admin":{"listen":"127.0.0.1:2019"}}`)
			}
			return out("Valid configuration\n")
		case "container":
			switch args[1] {
			case "ls":
				if contains(args, "name=^"+CaddyContainer+"-previous$") {
					if f.previous != nil {
						return out(f.previous.ID)
					}
					return out("")
				}
				var ids []string
				if f.caddy != nil {
					ids = append(ids, f.caddy.ID)
				}
				if f.foreign && !contains(args, "--filter") {
					ids = append(ids, "foreign-container")
				}
				return out(strings.Join(ids, "\n"))
			case "inspect":
				if f.caddy == nil || args[len(args)-1] != f.caddy.ID {
					return process.Result{}, fakeExit(1)
				}
				return out(f.caddy.inspect())
			case "rename":
				if args[3] == CaddyContainer {
					f.caddy, f.previous = f.previous, nil
				} else {
					f.previous, f.caddy = f.caddy, nil
				}
				return out("")
			case "stop":
				f.previous.Running = false
				return out("")
			case "rm":
				if f.previous != nil && args[len(args)-1] == f.previous.ID {
					f.previous = nil
					return out("")
				}
				f.caddy = nil
				return out("")
			case "start":
				f.caddy.Running = true
				return out("")
			}
		case "image":
			switch args[1] {
			case "tag":
				id := f.caddyImages[args[2]]
				if id == "" {
					return process.Result{}, fakeExit(1)
				}
				_, f.tagDigest, _ = strings.Cut(args[2], "@")
				f.caddyImages[args[3]] = id
				return out("")
			case "inspect":
				ref := args[len(args)-1]
				id := f.caddyImages[ref]
				if id == "" {
					id = f.caddyImages[digestRef(ref)]
				}
				var digests []string
				for r, i := range f.caddyImages {
					if i == id && strings.Contains(r, "@") {
						digests = append(digests, r)
					}
				}
				data, _ := json.Marshal(map[string]any{"Id": id, "RepoDigests": digests, "Descriptor": map[string]string{"digest": f.tagDigest}})
				return out(string(data))
			case "ls":
				var ids []string
				for ref, id := range f.caddyImages {
					if ref == args[len(args)-1] || args[len(args)-1] == "--no-trunc" {
						ids = append(ids, id)
					}
				}
				sort.Strings(ids)
				return out(strings.Join(ids, "\n"))
			case "rm":
				delete(f.caddyImages, args[2])
				return out("")
			}
		case "volume":
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
	for _, kind := range []string{"workload", "containerd", "cache", "config", "runtime-route", "mount", "unowned-removal", "app-data", "account", "symlink"} {
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
			case "unowned-removal":
				f.unownedRemoval = true
			case "app-data":
				f.write("/var/lib/vamos/apps/important", "data")
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
			if !f.active || f.caddy == nil || !f.caddy.Running {
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

// Ubuntu's unattended upgrades own the runtime package versions. Moving Docker
// forward or refreshing its packaged containerd config is expected, not drift.
func TestUbuntuRuntimeUpdatesAreNotOwnershipDrift(t *testing.T) {
	m, f := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Bootstrap(ctx, testConfig); err != nil {
		t.Fatal(err)
	}
	f.installed["docker.io"] = installedPackage{"installed", "29.7"}
	f.installed["containerd"] = installedPackage{"installed", "2.3"}
	f.write("/etc/containerd/config.toml", "containerd updated default")
	if _, err := m.Bootstrap(ctx, testConfig); err != nil {
		t.Fatalf("repeat setup rejected an Ubuntu update: %v", err)
	}
	m.Executor = []byte("executor-v2")
	if _, err := m.Upgrade(ctx, testConfig); err != nil {
		t.Fatalf("upgrade rejected an Ubuntu update: %v", err)
	}
	if _, err := m.Teardown(ctx, testConfig.Name, true); err != nil {
		t.Fatalf("teardown rejected an Ubuntu update: %v", err)
	}
	if len(f.installed) != 1 {
		t.Fatalf("updated packages were not purged: %+v", f.installed)
	}
}

// A resumed install must not upgrade owned packages itself or fail because the
// archive moved past the version APT planned originally.
func TestResumedInstallUsesCurrentArchiveWithoutUpgrading(t *testing.T) {
	m, f := newTestManager(t)
	f.failInstall = true
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
		t.Fatal("wanted APT failure")
	}
	f.archiveVersion = "newer"
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "apt-get") && strings.Contains(call, "docker.io=") {
			t.Fatalf("resume pinned an archive version: %s", call)
		}
	}
}

func TestFreshSetupRefusesPreexistingResources(t *testing.T) {
	for _, kind := range []string{"data", "binary", "caddy-binary", "package", "caddy-package", "state-dir", "account", "caddy-account", "unit-mask"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			switch kind {
			case "data":
				f.write("/var/lib/docker/existing", "important")
			case "binary":
				f.write("/usr/local/bin/docker", "existing")
			case "caddy-binary":
				f.write("/usr/bin/caddy", "existing")
			case "package":
				f.installed["containerd"] = installedPackage{"installed", "old"}
			case "caddy-package":
				f.installed["caddy"] = installedPackage{"installed", "2.6.2"}
			case "state-dir":
				f.write("/var/lib/vamos/unmanaged", "important")
			case "account":
				f.accounts["group:docker"] = "docker:x:999:someone"
			case "caddy-account":
				f.accounts["passwd:caddy"] = "caddy:x:998:998::/var/lib/caddy:/usr/sbin/nologin"
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
