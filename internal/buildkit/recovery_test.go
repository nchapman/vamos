package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func failBuild(t *testing.T, m Manager, f *fakeEngine) {
	t.Helper()
	f.calls = nil
	if _, err := m.Build(context.Background(), testHost, "df", "iid", nil); err == nil {
		t.Fatal("unsafe build succeeded")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "container start") || strings.HasPrefix(call, "buildx build") {
			t.Fatalf("started despite invalid state: %s", call)
		}
	}
}
func TestCrashAfterCreationResumesOnlyMatchingIntent(t *testing.T) {
	for _, kind := range []string{"network", "volume", "container"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newEngine(t)
			// Model process loss after Docker created the resource but before its
			// response/identity could be saved. Docker create itself never starts it.
			f.hook = func(ctx context.Context, a []string) (process.Result, error, bool) {
				if a[0] == kind && a[1] == "create" {
					f.hook = nil
					env := []string{"-u", "DOCKER_CONTEXT", "-u", "DOCKER_TLS", "-u", "DOCKER_TLS_VERIFY", "-u", "DOCKER_CERT_PATH", "-u", "BUILDKIT_HOST", "-u", "BUILDX_BUILDER", "DOCKER_HOST=unix:///var/run/docker.sock", "BUILDX_CONFIG=" + filepath.Join(f.root, "var/lib/vamos/buildkit/buildx"), "docker", "--host", "unix:///var/run/docker.sock"}
					_, err := f.run(ctx, "/usr/bin/env", append(env, a...), nil)
					if err != nil {
						t.Fatal(err)
					}
					return process.Result{}, errors.New("transport disconnected after creation"), true
				}
				return process.Result{}, nil, false
			}
			if _, err := m.Build(context.Background(), testHost, "df", "iid", nil); err == nil {
				t.Fatal("expected creation interruption")
			}
			before := f.state()
			switch kind {
			case "network":
				if before.NetworkID != "" {
					t.Fatal("already captured")
				}
			case "volume":
				if before.VolumeCreated != "" {
					t.Fatal("already captured")
				}
			case "container":
				if before.ContainerID != "" {
					t.Fatal("already captured")
				}
			}
			f.calls = nil
			runBuild(t, m)
			for _, call := range f.calls {
				if strings.HasPrefix(call, kind+" create") {
					t.Fatal("recreated instead of recovering")
				}
			}
			if f.state().Nonce != before.Nonce {
				t.Fatal("lost intent")
			}
		})
	}
}
func TestReplacementsAndForeignShapeRefused(t *testing.T) {
	tests := map[string]func(*fakeEngine){
		"container ID":    func(f *fakeEngine) { f.container["Id"] = strings.Repeat("d", 64) },
		"network ID":      func(f *fakeEngine) { f.network["Id"] = strings.Repeat("d", 64) },
		"volume creation": func(f *fakeEngine) { f.volume["CreatedAt"] = "2026-02-01T00:00:00Z" },
		"volume labels":   func(f *fakeEngine) { f.volume["Labels"] = map[string]string{} },
		"foreign endpoint": func(f *fakeEngine) {
			f.network["Containers"] = map[string]any{strings.Repeat("f", 64): map[string]any{"Name": "foreign"}}
		},
		"network IPv6": func(f *fakeEngine) { f.network["EnableIPv6"] = true },
		"network IPAM": func(f *fakeEngine) {
			f.network["IPAM"] = ipam{Driver: "default", Config: []ipamConfig{{Subnet: "172.30.0.0/16", Gateway: "172.30.0.1"}}}
		},
		"container env":  func(f *fakeEngine) { f.container["Config"].(map[string]any)["Env"] = []string{"HTTP_PROXY=secret"} },
		"container user": func(f *fakeEngine) { f.container["Config"].(map[string]any)["User"] = "1000:1000" },
		"container entitlement": func(f *fakeEngine) {
			f.container["Config"].(map[string]any)["Cmd"] = []string{"--allow-insecure-entitlement=network.host"}
		},
		"host bind": func(f *fakeEngine) {
			f.container["HostConfig"].(map[string]any)["Binds"] = []string{"/var/run/docker.sock:/var/run/docker.sock"}
		},
		"restart policy": func(f *fakeEngine) {
			f.container["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "always"}
		},
		"host config captured default changed": func(f *fakeEngine) {
			f.container["HostConfig"].(map[string]any)["LogConfig"] = map[string]any{"Type": "json-file"}
		},
		"missing captured container": func(f *fakeEngine) { f.container = nil },
		"missing captured network":   func(f *fakeEngine) { f.network = nil },
		"missing captured volume":    func(f *fakeEngine) { f.volume = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) { m, f := newEngine(t); runBuild(t, m); mutate(f); failBuild(t, m, f) })
	}
}

// Docker updates restart the daemon and can renormalize stored defaults Vamos
// never sets. Those must not make every subsequent build fail.
func TestDockerNormalizationIsNotDrift(t *testing.T) {
	tests := map[string]func(*fakeEngine){
		"unset host defaults": func(f *fakeEngine) {
			host := f.container["HostConfig"].(map[string]any)
			host["ShmSize"] = 67108864
			host["ConsoleSize"] = []int{0, 0}
			host["CgroupnsMode"] = "private"
		},
		"unset config defaults": func(f *fakeEngine) {
			f.container["Config"].(map[string]any)["Hostname"] = containerID[:12]
			f.container["Config"].(map[string]any)["StopTimeout"] = nil
		},
		"empty IPAM maps": func(f *fakeEngine) {
			f.network["IPAM"] = map[string]any{"Driver": "default", "Options": map[string]string{}, "Config": []map[string]any{{"Subnet": "172.29.0.0/16", "Gateway": "172.29.0.1", "AuxiliaryAddresses": map[string]string{}}}}
		},
		"volume time zone": func(f *fakeEngine) { f.volume["CreatedAt"] = "2026-01-01T01:00:00+01:00" },
		"volume precision": func(f *fakeEngine) { f.volume["CreatedAt"] = "2026-01-01T00:00:00.123456789Z" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) { m, f := newEngine(t); runBuild(t, m); mutate(f); runBuild(t, m) })
	}
}
func TestInventoriesFailClosed(t *testing.T) {
	for _, kind := range []string{"container", "network", "volume"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
				if a[0] == kind && a[1] == "ls" {
					return process.Result{}, errors.New("daemon unavailable, not empty"), true
				}
				return process.Result{}, nil, false
			}
			failBuild(t, m, f)
			for _, call := range f.calls {
				if strings.Contains(call, " create ") {
					t.Fatal("created after failed inventory")
				}
			}
		})
	}
}
func TestGuardRepairStopsFirstAndPreservesUnknownRules(t *testing.T) {
	for _, mode := range []string{"missing", "late", "inspection failure", "installation failure", "ineffective installation"} {
		t.Run(mode, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			f.container["State"] = map[string]any{"Running": true}
			key := "/usr/sbin/iptables:PREROUTING"
			unknown := []string{"-i", bridge, "-j", "ACCEPT"}
			f.rules[key] = append([][]string{unknown}, f.rules[key]...)
			if mode == "missing" {
				f.rules[key] = [][]string{unknown}
			}
			stopped := false
			f.calls = nil
			f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
				if a[0] == "container" && a[1] == "stop" {
					stopped = true
				}
				if strings.HasSuffix(a[0], "tables") {
					if !stopped || running(f) {
						t.Fatal("firewall accessed before verified daemon stopped")
					}
					if mode == "inspection failure" && a[5] == "-S" {
						return process.Result{}, errors.New("unavailable"), true
					}
					if mode == "installation failure" && a[5] == "-I" {
						return process.Result{}, errors.New("unavailable"), true
					}
					if mode == "ineffective installation" && a[5] == "-I" {
						return process.Result{}, nil, true
					}
				}
				return process.Result{}, nil, false
			}
			if strings.Contains(mode, "failure") || mode == "ineffective installation" {
				failBuild(t, m, f)
			} else {
				runBuild(t, m)
				f.assertGuards()
				if !reflect.DeepEqual(f.rules[key][1], unknown) {
					t.Fatal("unknown rule changed")
				}
			}
			if running(f) || !stopped {
				t.Fatal("repair path did not stop")
			}
		})
	}
}
func TestBuildxMetadataCannotRedirect(t *testing.T) {
	for _, kind := range []string{"endpoint", "driver", "options", "flags", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newEngine(t)
			runBuild(t, m)
			s := f.state()
			path := filepath.Join(f.root, "var/lib/vamos/buildkit/buildx/instances", s.Builder)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var group map[string]any
			if json.Unmarshal(raw, &group) != nil {
				t.Fatal("json")
			}
			node := group["Nodes"].([]any)[0].(map[string]any)
			switch kind {
			case "endpoint":
				node["Endpoint"] = "tcp://external.invalid:1234"
			case "driver":
				group["Driver"] = "docker-container"
			case "options":
				node["DriverOpts"] = map[string]any{"cacert": "/tmp/cert"}
			case "flags":
				node["Flags"] = []string{"--allow-insecure-entitlement=network.host"}
			case "unknown":
				group["FutureField"] = true
			}
			raw, _ = json.Marshal(group)
			if os.WriteFile(path, raw, 0600) != nil {
				t.Fatal("write")
			}
			failBuild(t, m, f)
		})
	}
}
func TestSnapshotterIsIntentNotPerBuildSetting(t *testing.T) {
	m, f := newEngine(t)
	m.Snapshotter = "native"
	runBuild(t, m)
	if f.state().Snapshotter != "native" {
		t.Fatal("missing snapshotter intent")
	}
	m.Snapshotter = ""
	runBuild(t, m)
	m.Snapshotter = "auto"
	failBuild(t, m, f)
}
func TestReceiptNamespaceAndCorruptionRefused(t *testing.T) {
	for _, kind := range []string{"empty namespace", "unknown JSON", "corrupt JSON", "wrong host", "symlink receipt", "symlink namespace", "public receipt"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newEngine(t)
			dir := filepath.Join(f.root, "var/lib/vamos/buildkit")
			path := filepath.Join(dir, "state.json")
			switch kind {
			case "empty namespace":
				if os.MkdirAll(dir, 0700) != nil {
					t.Fatal("mkdir")
				}
			case "symlink namespace":
				if os.MkdirAll(filepath.Dir(dir), 0700) != nil || os.Symlink(f.root, dir) != nil {
					t.Fatal("symlink")
				}
			default:
				runBuild(t, m)
				switch kind {
				case "unknown JSON":
					raw, _ := os.ReadFile(path)
					raw = append([]byte(`{"Unknown":true,`), raw[1:]...)
					if os.WriteFile(path, raw, 0600) != nil {
						t.Fatal("write")
					}
				case "corrupt JSON":
					if os.WriteFile(path, []byte(`{`), 0600) != nil {
						t.Fatal("write")
					}
				case "wrong host":
					s := f.state()
					s.Host = strings.Repeat("0", 32)
					if save(dir, s) != nil {
						t.Fatal("save")
					}
				case "symlink receipt":
					if os.Rename(path, path+".saved") != nil || os.Symlink(path+".saved", path) != nil {
						t.Fatal("symlink")
					}
				case "public receipt":
					if os.Chmod(path, 0644) != nil {
						t.Fatal("chmod")
					}
				}
			}
			failBuild(t, m, f)
			if len(f.calls) != 0 {
				t.Fatal("Docker contacted with invalid namespace")
			}
		})
	}
}
func TestUnknownResourcesNeverAdopted(t *testing.T) {
	m, f := newEngine(t)
	f.network = map[string]any{"Id": networkID, "Name": "vamos-buildkit-" + testHost + "-net", "Driver": "bridge", "Scope": "local", "Labels": map[string]string{}}
	failBuild(t, m, f)
	for _, call := range f.calls {
		if strings.Contains(call, "network create") || strings.Contains(call, "network rm") {
			t.Fatal("unknown network changed")
		}
	}
}
func TestUncertainStartAndStopFallback(t *testing.T) {
	for _, mode := range []string{"start failure", "stop failure", "cleanup unavailable"} {
		t.Run(mode, func(t *testing.T) {
			m, f := newEngine(t)
			attempts := 0
			f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
				if mode == "start failure" && a[0] == "container" && a[1] == "start" {
					f.container["State"] = map[string]any{"Running": true}
					s := f.state()
					f.container["NetworkSettings"] = map[string]any{"Networks": map[string]any{s.Network: map[string]any{"NetworkID": networkID}}}
					return process.Result{}, errors.New("response lost"), true
				}
				if a[0] == "container" && (a[1] == "stop" || a[1] == "kill") {
					attempts++
					if mode == "cleanup unavailable" || mode == "stop failure" && a[1] == "stop" {
						return process.Result{}, errors.New("stop unavailable"), true
					}
				}
				return process.Result{}, nil, false
			}
			_, err := m.Build(context.Background(), testHost, "df", "iid", nil)
			if mode == "stop failure" {
				if err != nil || attempts != 2 || running(f) {
					t.Fatalf("failed kill fallback: %v", err)
				}
			} else if err == nil {
				t.Fatal("lost failure")
			}
			if mode == "start failure" && (attempts != 1 || running(f)) {
				t.Fatal("uncertain start left running")
			}
		})
	}
}
