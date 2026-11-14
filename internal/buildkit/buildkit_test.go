package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

const testHost = "0123456789abcdef0123456789abcdef"

var containerID = strings.Repeat("a", 64)
var networkID = strings.Repeat("b", 64)
var imageID = "sha256:" + strings.Repeat("c", 64)

// fakeEngine is an external CLI model: it persists independently of receipts,
// emits Docker inventory/inspect JSON and implements ordered iptables chains.
// No command ever falls through to a real executable.
type fakeEngine struct {
	t           *testing.T
	root        string
	calls       []string
	container   map[string]any
	network     map[string]any
	volume      map[string]any
	rules       map[string][][]string
	hook        func(context.Context, []string) (process.Result, error, bool)
	buildOutput process.Result
	archive     []byte
}

func newEngine(t *testing.T) (Manager, *fakeEngine) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("privileged build-service filesystem ownership is Unix-only")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEngine{t: t, root: root, rules: map[string][][]string{}, buildOutput: process.Result{Stdout: []byte("loaded image"), Stderr: []byte("build progress")}}
	return Manager{Root: root, Run: f.run}, f
}
func jsonResult(v any) process.Result { raw, _ := json.Marshal(v); return process.Result{Stdout: raw} }
func (f *fakeEngine) state() receipt {
	raw, err := os.ReadFile(filepath.Join(f.root, "var/lib/soar/buildkit/state.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var s receipt
	if json.Unmarshal(raw, &s) != nil {
		f.t.Fatal("invalid state")
	}
	return s
}
func (f *fakeEngine) run(ctx context.Context, exe string, args []string, stdin []byte) (process.Result, error) {
	f.t.Helper()
	if exe != "/usr/bin/env" || len(args) < 15 {
		f.t.Fatalf("unconstrained command: %s %v", exe, args)
	}
	for i, key := range []string{"DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "BUILDKIT_HOST", "BUILDX_BUILDER"} {
		if args[2*i] != "-u" || args[2*i+1] != key {
			f.t.Fatalf("inherited endpoint selector %s", key)
		}
	}
	for _, expected := range []string{"DOCKER_HOST=unix:///var/run/docker.sock", "BUILDX_CONFIG=" + filepath.Join(f.root, "var/lib/soar/buildkit/buildx")} {
		if !contains(args, expected) {
			f.t.Fatalf("missing environment %s", expected)
		}
	}
	command := args[14]
	a := args[15:]
	if command == "docker" {
		if len(a) < 2 || a[0] != "--host" || a[1] != "unix:///var/run/docker.sock" {
			f.t.Fatal("missing explicit local daemon")
		}
		a = a[2:]
	} else {
		a = append([]string{command}, a...)
	}
	f.calls = append(f.calls, strings.Join(a, " "))
	if f.hook != nil {
		if r, err, ok := f.hook(ctx, a); ok {
			return r, err
		}
	}
	if err := ctx.Err(); err != nil {
		return process.Result{}, err
	}
	if strings.HasSuffix(command, "tables") {
		return f.firewall(command, a[1:])
	}
	if len(a) < 2 {
		f.t.Fatalf("unexpected command %v", a)
	}
	kind, op := a[0], a[1]
	switch kind + " " + op {
	case "container ls":
		if f.container == nil {
			return process.Result{}, nil
		}
		return jsonResult(map[string]any{"ID": f.container["Id"], "Names": strings.TrimPrefix(f.container["Name"].(string), "/"), "Image": Image}), nil
	case "network ls":
		if f.network == nil {
			return process.Result{}, nil
		}
		return jsonResult(map[string]any{"ID": f.network["Id"], "Name": f.network["Name"], "Driver": "bridge"}), nil
	case "volume ls":
		if f.volume == nil {
			return process.Result{}, nil
		}
		return jsonResult(map[string]any{"Name": f.volume["Name"], "Driver": "local"}), nil
	case "container inspect":
		if f.container == nil || a[2] != f.container["Id"] {
			return process.Result{}, errors.New("not found")
		}
		return jsonResult([]any{f.container}), nil
	case "network inspect":
		return jsonResult([]any{f.network}), nil
	case "volume inspect":
		return jsonResult([]any{f.volume}), nil
	case "image pull":
		return process.Result{}, nil
	case "image inspect":
		return jsonResult([]any{map[string]any{"Id": imageID, "RepoDigests": []string{"moby/buildkit@" + strings.Split(Image, "@")[1]}, "Config": map[string]any{"Env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, "Labels": nil}}}), nil
	case "network create":
		s := f.state()
		f.network = map[string]any{"Id": networkID, "Name": s.Network, "Driver": "bridge", "Scope": "local", "EnableIPv4": true, "EnableIPv6": false, "IPAM": ipam{Driver: "default", Config: []ipamConfig{{Subnet: "172.29.0.0/16", Gateway: "172.29.0.1"}}}, "Options": map[string]string{"com.docker.network.bridge.name": bridge, "com.docker.network.bridge.enable_icc": "false"}, "Labels": s.labels("network"), "Containers": map[string]any{}}
		return process.Result{Stdout: []byte(networkID + "\n")}, nil
	case "volume create":
		s := f.state()
		f.volume = map[string]any{"Name": s.Volume, "Driver": "local", "Scope": "local", "CreatedAt": "2026-01-01T00:00:00Z", "Mountpoint": "/var/lib/docker/volumes/" + s.Volume + "/_data", "Labels": s.labels("volume"), "Options": nil}
		return process.Result{Stdout: []byte(s.Volume + "\n")}, nil
	case "container create":
		f.makeContainer(a)
		return process.Result{Stdout: []byte(containerID + "\n")}, nil
	case "container start":
		f.assertGuards()
		if a[2] != f.container["Id"] {
			f.t.Fatal("start did not use exact ID")
		}
		f.container["State"] = map[string]any{"Running": true}
		s := f.state()
		f.container["NetworkSettings"] = map[string]any{"Networks": map[string]any{s.Network: map[string]any{"NetworkID": networkID, "Aliases": []string{s.Container, containerID[:12]}, "IPAddress": "172.29.0.2", "Gateway": "172.29.0.1"}}}
		f.network["Containers"] = map[string]any{containerID: map[string]any{"Name": s.Container, "IPv4Address": "172.29.0.2/16"}}
		return process.Result{}, nil
	case "container stop", "container kill":
		if a[len(a)-1] != f.container["Id"] {
			f.t.Fatal("stop did not use exact ID")
		}
		f.container["State"] = map[string]any{"Running": false}
		f.network["Containers"] = map[string]any{}
		return process.Result{}, nil
	case "buildx create":
		s := f.state()
		want := []string{"buildx", "create", "--name", s.Builder, "--node", s.Builder + "0", "--driver", "remote", "docker-container://" + s.Container}
		if !reflect.DeepEqual(a, want) {
			f.t.Fatalf("unsafe builder creation: %v", a)
		}
		// External Buildx store shape, intentionally not marshaled from our parser type.
		group := map[string]any{"Name": s.Builder, "Driver": "remote", "Dynamic": false, "DockerContext": false, "Nodes": []any{map[string]any{"Name": s.Builder + "0", "Endpoint": "docker-container://" + s.Container, "Platforms": nil, "DriverOpts": nil, "Flags": nil, "Files": nil}}}
		p := filepath.Join(f.root, "var/lib/soar/buildkit/buildx/instances")
		if os.MkdirAll(p, 0700) != nil {
			f.t.Fatal("mkdir")
		}
		raw, _ := json.Marshal(group)
		if os.WriteFile(filepath.Join(p, s.Builder), raw, 0600) != nil {
			f.t.Fatal("write")
		}
		return process.Result{Stdout: []byte(s.Builder + "\n")}, nil
	case "buildx build":
		f.archive = append([]byte(nil), stdin...)
		return f.buildOutput, nil
	default:
		f.t.Fatalf("unexpected CLI command %v", a)
	}
	return process.Result{}, errors.New("unreachable")
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func (f *fakeEngine) makeContainer(args []string) {
	s := f.state()
	for _, pair := range [][2]string{{"--name", s.Container}, {"--network", networkID}, {"--user", "0:0"}, {"--workdir", "/"}, {"--entrypoint", "buildkitd"}, {"--mount", "type=volume,source=" + s.Volume + ",target=/var/lib/buildkit"}, {"--ipc", "private"}, {"--log-driver", "local"}, {"--security-opt", "label=disable"}} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == pair[0] && args[i+1] == pair[1] {
				found = true
			}
		}
		if !found {
			f.t.Fatalf("missing container setting %v: %v", pair, args)
		}
	}
	if !contains(args, "--privileged") || !contains(args, "--restart=no") || args[len(args)-2] != Image || args[len(args)-1] != "--oci-worker-snapshotter="+s.Snapshotter {
		f.t.Fatalf("unsafe container creation: %v", args)
	}
	cmd := []string{"--oci-worker-snapshotter=" + s.Snapshotter}
	// AttachStdout/AttachStderr are Docker create's normal CLI defaults. A
	// never-started container has no allocated NetworkID yet.
	f.container = map[string]any{
		"Id": containerID, "Name": "/" + s.Container, "Image": imageID, "Path": "buildkitd", "Args": cmd, "State": map[string]any{"Running": false},
		"Config":          map[string]any{"Image": Image, "User": "0:0", "WorkingDir": "/", "Hostname": containerID[:12], "Entrypoint": []string{"buildkitd"}, "Cmd": cmd, "Env": s.ImageEnv, "Labels": s.labels("container"), "AttachStdout": true, "AttachStderr": true, "Volumes": map[string]any{"/var/lib/buildkit": map[string]any{}}},
		"HostConfig":      map[string]any{"Privileged": true, "RestartPolicy": map[string]any{"Name": "no", "MaximumRetryCount": 0}, "NetworkMode": networkID, "IpcMode": "private", "SecurityOpt": []string{"label=disable"}, "Mounts": []any{map[string]any{"Type": "volume", "Source": s.Volume, "Target": "/var/lib/buildkit"}}, "LogConfig": map[string]any{"Type": "local", "Config": map[string]any{}}},
		"Mounts":          []any{map[string]any{"Type": "volume", "Name": s.Volume, "Source": s.VolumeMountpoint, "Destination": "/var/lib/buildkit", "Driver": "local", "Mode": "z", "RW": true, "Propagation": ""}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{s.Network: map[string]any{"NetworkID": "", "Aliases": []string{s.Container, containerID[:12]}}}},
	}
}
func (f *fakeEngine) firewall(exe string, a []string) (process.Result, error) {
	if len(a) < 6 || !reflect.DeepEqual(a[:4], []string{"-w", "5", "-t", "mangle"}) {
		f.t.Fatalf("unexpected firewall args %v", a)
	}
	op, chain := a[4], a[5]
	key := exe + ":" + chain
	switch op {
	case "-S":
		lines := []string{"-P " + chain + " ACCEPT"}
		for _, r := range f.rules[key] {
			tokens := append([]string(nil), r...)
			for i := range tokens {
				if i > 0 && tokens[i-1] == "--comment" {
					tokens[i] = `"` + tokens[i] + `"`
				}
			}
			lines = append(lines, "-A "+chain+" "+strings.Join(tokens, " "))
		}
		return process.Result{Stdout: []byte(strings.Join(lines, "\n") + "\n")}, nil
	case "-I":
		if a[6] != "1" {
			f.t.Fatal("not first")
		}
		f.rules[key] = append([][]string{append([]string(nil), a[7:]...)}, f.rules[key]...)
	case "-D":
		found := false
		for i, r := range f.rules[key] {
			if reflect.DeepEqual(r, a[6:]) {
				f.rules[key] = append(f.rules[key][:i], f.rules[key][i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return process.Result{}, errors.New("rule missing")
		}
	default:
		f.t.Fatalf("unexpected firewall operation %s", op)
	}
	return process.Result{}, nil
}
func (f *fakeEngine) assertGuards() {
	f.t.Helper()
	b := build{s: f.state()}
	for _, exe := range []string{"/usr/sbin/iptables", "/usr/sbin/ip6tables"} {
		for _, chain := range []string{"PREROUTING", "FORWARD"} {
			index := 0
			for _, g := range b.guards() {
				if g.chain != chain {
					continue
				}
				r := f.rules[exe+":"+chain]
				if len(r) <= index || !reflect.DeepEqual(r[index], g.args) {
					f.t.Fatal("container started without early guards")
				}
				index++
			}
		}
	}
}
func runBuild(t *testing.T, m Manager) process.Result {
	t.Helper()
	r, err := m.Build(context.Background(), testHost, "Dockerfile", "/tmp/image.iid", []byte("tar archive"))
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	return r
}
func running(f *fakeEngine) bool {
	return f.container != nil && f.container["State"].(map[string]any)["Running"] == true
}
func TestBuildNormalShapeAndCache(t *testing.T) {
	m, f := newEngine(t)
	r := runBuild(t, m)
	if !reflect.DeepEqual(r, f.buildOutput) || string(f.archive) != "tar archive" {
		t.Fatal("build output/stdin not preserved")
	}
	if running(f) {
		t.Fatal("daemon left running")
	}
	expected := "buildx build --builder soar-" + testHost + " --load --network default --iidfile /tmp/image.iid --file Dockerfile -"
	if !contains(f.calls, expected) {
		t.Fatalf("missing build command: %v", f.calls)
	}
	for _, call := range f.calls {
		for _, bad := range []string{"network.host", "security.insecure", "--allow", "--network host", "docker-container --", "/var/run/docker.sock:/", " volume rm", " network rm", " container rm"} {
			if strings.Contains(call, bad) {
				t.Fatalf("forbidden command: %s", call)
			}
		}
	}
	first := f.state()
	f.calls = nil
	runBuild(t, m)
	second := f.state()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("receipt/cache identity changed")
	}
	for _, call := range f.calls {
		if strings.Contains(call, " create ") || strings.Contains(call, "image pull") {
			t.Fatalf("recreated retained resource: %s", call)
		}
	}
}
func TestInfrastructureErrorIsValueFree(t *testing.T) {
	m, f := newEngine(t)
	f.hook = func(_ context.Context, a []string) (process.Result, error, bool) {
		if reflect.DeepEqual(a[:2], []string{"container", "ls"}) {
			return process.Result{Stderr: []byte("SECRET")}, errors.New("SECRET"), true
		}
		return process.Result{}, nil, false
	}
	r, err := m.Build(context.Background(), testHost, "df", "iid", nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || len(r.Stdout)+len(r.Stderr) != 0 {
		t.Fatal("leaked infrastructure error")
	}
}
func TestBuildFailureAndCancellationStop(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			m, f := newEngine(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleanupLive := false
			f.hook = func(c context.Context, a []string) (process.Result, error, bool) {
				if a[0] == "buildx" && a[1] == "build" {
					if cancelled {
						cancel()
					}
					return process.Result{Stderr: []byte("secret build output")}, errors.New("secret failure"), true
				}
				if a[0] == "container" && a[1] == "stop" {
					_, bounded := c.Deadline()
					cleanupLive = c.Err() == nil && bounded
				}
				return process.Result{}, nil, false
			}
			r, err := m.Build(ctx, testHost, "df", "iid", nil)
			if err == nil || strings.Contains(err.Error(), "secret") || string(r.Stderr) != "secret build output" || running(f) || !cleanupLive {
				t.Fatalf("unsafe cleanup/output: %v %v", err, f.calls)
			}
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost")
			}
		})
	}
}
