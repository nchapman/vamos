package buildbundle

import (
	"context"
	"debug/elf"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/buildmeta"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	meta, err := os.ReadFile("../buildmeta/buildmeta.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go.mod":                          "module github.com/nchapman/vamos\n\ngo 1.25.0\n",
		"go.sum":                          "",
		"internal/buildmeta/buildmeta.go": string(meta),
		"cmd/vamos/main.go":               "package main\nimport (\"fmt\";\"github.com/nchapman/vamos/internal/buildmeta\")\nfunc main(){fmt.Println(buildmeta.Version())}\n",
	} {
		write(t, filepath.Join(root, name), data)
	}
	return root
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildProducesMatchingBundle(t *testing.T) {
	root := fixture(t)
	out := filepath.Join(t.TempDir(), "bundle")
	if err := Build(context.Background(), root, out, "v1.2.3", io.Discard); err != nil {
		t.Fatal(err)
	}
	files, err := snapshot(root, filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := buildmeta.Encode(identity(files, runtime.Version(), "v1.2.3"), "v1.2.3")
	for _, target := range targets() {
		path := filepath.Join(out, target.name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = buildmeta.Check(stamp, data); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target.name, "vamos-linux-") {
			binary, err := elf.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			want := elf.EM_X86_64
			if target.arch == "arm64" {
				want = elf.EM_AARCH64
			}
			if binary.Machine != want {
				t.Fatal(binary.Machine)
			}
			binary.Close()
		}
	}
	version, err := exec.Command(filepath.Join(out, targets()[0].name), "version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "v1.2.3 (build "+identity(files, runtime.Version(), "v1.2.3")+")") {
		t.Fatalf("version %s: %v", version, err)
	}
	// Replacing a complete bundle is supported, including on Windows.
	if err = Build(context.Background(), root, out, "v1.2.4", io.Discard); err != nil {
		t.Fatal(err)
	}
	newer, err := os.ReadFile(filepath.Join(out, "vamos-linux-arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if buildmeta.Check(stamp, newer) == nil {
		t.Fatal("old client accepted a new bundle")
	}
}

func TestIdentityTracksAllCapturedInputs(t *testing.T) {
	files := map[string][]byte{
		"cmd/vamos/main.go":   []byte("source"),
		"internal/ssh/app.sh": []byte("embedded"),
		"go.mod":              []byte("module"),
		"go.sum":              []byte("sums"),
	}
	old := identity(files, "go1.27.1", "dev")
	if old != identity(files, "go1.27.1", "dev") {
		t.Fatal("unstable identity")
	}
	for name := range files {
		original := files[name]
		files[name] = append([]byte("changed"), original...)
		if old == identity(files, "go1.27.1", "dev") {
			t.Fatal("missed", name)
		}
		files[name] = original
	}
	if old == identity(files, "go1.27.2", "dev") || old == identity(files, "go1.27.1", "release") {
		t.Fatal("missed recipe input")
	}
}

func TestSnapshotDoesNotDependOnGitOrLaterEdits(t *testing.T) {
	root := fixture(t)
	dest := filepath.Join(t.TempDir(), "snapshot")
	first, err := snapshot(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ".git", "HEAD"), "different checkout")
	second, err := snapshot(root, filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if identity(first, "go1", "dev") != identity(second, "go1", "dev") {
		t.Fatal("Git affected build")
	}
	write(t, filepath.Join(root, "cmd", "vamos", "main.go"), "edited after snapshot")
	captured, err := os.ReadFile(filepath.Join(dest, "cmd", "vamos", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(captured) != string(first["cmd/vamos/main.go"]) {
		t.Fatal("source edit affected snapshot")
	}
}

func TestFailedBuildDoesNotPublish(t *testing.T) {
	root := fixture(t)
	out := t.TempDir()
	dest := filepath.Join(out, targets()[0].name)
	write(t, dest, "old-binary")
	write(t, filepath.Join(root, "cmd/vamos/main.go"), "this is not Go")
	if err := Build(context.Background(), root, out, "dev", io.Discard); err == nil {
		t.Fatal("broken build succeeded")
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "old-binary" {
		t.Fatalf("output changed: %s %v", data, err)
	}
}

func TestLocalModuleReplacementRefused(t *testing.T) {
	root := fixture(t)
	write(t, filepath.Join(root, "go.mod"), "module github.com/nchapman/vamos\ngo 1.25.0\nreplace example.com/module => ../outside\n")
	err := Build(context.Background(), root, t.TempDir(), "dev", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "local module replacements") {
		t.Fatal(err)
	}
}

func TestBuildEnvironment(t *testing.T) {
	env := buildEnvironment([]string{"PATH=path", "GOPROXY=proxy", "GOFLAGS=-race -overlay=/outside", "GOENV=bad", "GOWORK=/outside", "GOOS=other", "GOARCH=other", "CGO_ENABLED=1", "GOAMD64=v4", "GOARM64=v9.0", "GOEXPERIMENT=bad", "GOTOOLCHAIN=auto"})
	got := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if _, ok := got[k]; ok {
			t.Fatal("duplicate", k)
		}
		got[k] = v
	}
	for key, want := range map[string]string{"PATH": "path", "GOPROXY": "proxy", "GOFLAGS": "", "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOEXPERIMENT": "", "GO111MODULE": "on", "GOFIPS140": "off", "CGO_ENABLED": "0", "GOAMD64": "v1", "GOARM64": "v8.0"} {
		if got[key] != want {
			t.Fatalf("%s=%q", key, got[key])
		}
	}
	if _, ok := got["GOOS"]; ok {
		t.Fatal("inherited GOOS")
	}
	if _, ok := got["GOARCH"]; ok {
		t.Fatal("inherited GOARCH")
	}
}

func TestBuildForCrossTargetBundle(t *testing.T) {
	root := fixture(t)
	out := filepath.Join(t.TempDir(), "win-bundle")
	if err := BuildFor(context.Background(), root, out, "v2.0.0", "windows", "amd64", io.Discard); err != nil {
		t.Fatal(err)
	}
	files, err := snapshot(root, filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := buildmeta.Encode(identity(files, runtime.Version(), "v2.0.0"), "v2.0.0")
	for _, name := range []string{"vamos.exe", "vamos-linux-amd64", "vamos-linux-arm64"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if err := buildmeta.Check(stamp, data); err != nil {
			t.Fatalf("verify %s: %v", name, err)
		}
	}
	if err := BuildFor(context.Background(), root, t.TempDir(), "v2.0.0", "../bad", "amd64", io.Discard); err == nil {
		t.Fatal("accepted invalid target OS")
	}
}
