package buildcontext

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func put(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func unpack(t *testing.T, b []byte) map[string]string {
	t.Helper()
	result := map[string]string{}
	r := tar.NewReader(bytes.NewReader(b))
	for {
		h, e := r.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r)
		if e != nil {
			t.Fatal(e)
		}
		result[h.Name] = string(data)
	}
	return result
}
func TestArchive(t *testing.T) {
	root := t.TempDir()
	for name, text := range map[string]string{
		"ctx/Dockerfile": "FROM scratch\n", "ctx/.dockerignore": "\ufeff# comment\nDockerfile\n.dockerignore\ncache\n!cache/deep/keep\n**/*.secret\n!.git/config\n!nested/.vamos/key\n",
		"ctx/hello": "hello world", "ctx/cache/deep/keep": "keep", "ctx/cache/deep/drop": "drop", "ctx/nested/a.secret": "secret", "ctx/.git/config": "private", "ctx/nested/.vamos/key": "private",
	} {
		put(t, root, name, text)
	}
	b, e := Archive(filepath.Join(root, "vamos.yaml"), config.Build{Context: "ctx"})
	if e != nil {
		t.Fatal(e)
	}
	files := unpack(t, b)
	for name, want := range map[string]string{"hello": "hello world", "cache/deep/keep": "keep", "Dockerfile": "FROM scratch\n"} {
		if files[name] != want {
			t.Fatalf("%s: %q", name, files[name])
		}
	}
	if _, ok := files[".dockerignore"]; !ok {
		t.Fatal("ignore not forced")
	}
	for _, name := range []string{"cache/deep/drop", "nested/a.secret", ".git/config", "nested/.vamos/key"} {
		if _, ok := files[name]; ok {
			t.Fatalf("included %s", name)
		}
	}
}
func TestSpecificIgnore(t *testing.T) {
	root := t.TempDir()
	put(t, root, "sub/Buildfile", "FROM scratch")
	put(t, root, ".dockerignore", "keep\n")
	put(t, root, "sub/Buildfile.dockerignore", "drop\nsub\n")
	put(t, root, "keep", "yes")
	put(t, root, "drop", "no")
	b, e := Archive(filepath.Join(root, "config"), config.Build{Dockerfile: "sub/Buildfile"})
	if e != nil {
		t.Fatal(e)
	}
	files := unpack(t, b)
	for _, name := range []string{"keep", "sub/Buildfile", "sub/Buildfile.dockerignore"} {
		if _, ok := files[name]; !ok {
			t.Fatal(name)
		}
	}
	if _, ok := files["drop"]; ok {
		t.Fatal("drop included")
	}
}
func TestSymlinks(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch")
	put(t, root, "target", "data")
	if e := os.Symlink("target", filepath.Join(root, "safe")); e != nil {
		t.Skipf("symlinks unavailable: %v", e)
	}
	cfg := filepath.Join(root, "config")
	if _, e := Archive(cfg, config.Build{}); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	put(t, outside, "secret", "secret")
	if e := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e := Archive(cfg, config.Build{}); e == nil {
		t.Fatal("accepted escaping link")
	}
	if _, e := Archive(cfg, config.Build{Dockerfile: "escape"}); e == nil {
		t.Fatal("accepted escaping Dockerfile")
	}
	put(t, root, ".dockerignore", "escape\n")
	if _, e := Archive(cfg, config.Build{}); e != nil {
		t.Fatal(e)
	}
}
func TestLimitsAndPaths(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch")
	for _, p := range []string{"../Dockerfile", "/Dockerfile", `C:\Dockerfile`, ".git/Dockerfile"} {
		if _, e := Archive(filepath.Join(root, "config"), config.Build{Dockerfile: p}); e == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	f, e := os.Create(filepath.Join(root, "large"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(MaxBytes); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = Archive(filepath.Join(root, "config"), config.Build{}); e == nil || !strings.Contains(e.Error(), "limit") {
		t.Fatalf("expected explicit limit: %v", e)
	}
}
