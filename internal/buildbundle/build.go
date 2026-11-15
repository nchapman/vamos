// Package buildbundle builds all executables from one private input snapshot.
package buildbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/buildmeta"
)

// Change this domain when changing the compiler recipe below. Target-specific
// GOOS/GOARCH differ by design; all targets otherwise use the same recipe.
const profile = "vamos-bundle-inputs-v1;trimpath;buildvcs=false;mod=readonly;cgo=0;fips=off;amd64=v1;arm64=v8.0"

type target struct{ name, os, arch string }

func targets() []target {
	name := "vamos"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return []target{
		{name, runtime.GOOS, runtime.GOARCH},
		{"vamos-linux-amd64", "linux", "amd64"},
		{"vamos-linux-arm64", "linux", "arm64"},
	}
}

// Build leaves existing outputs alone until every executable is built and
// verified. Publication is per-file, not atomic for the directory: the runtime
// pairing guard rejects any mixed bundle after interrupted publication.
func Build(ctx context.Context, root, output, label string, log io.Writer) error {
	if !buildmeta.ValidLabel(label) {
		return fmt.Errorf("version must be 1–64 ASCII letters, digits, '.', '+', '_', or '-', starting with a letter or digit")
	}
	scratch, err := os.MkdirTemp("", "vamos-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	source := filepath.Join(scratch, "source")
	files, err := snapshot(root, source)
	if err != nil {
		return err
	}
	env := buildEnvironment(os.Environ())
	version, err := commandOutput(ctx, source, env, "env", "GOVERSION")
	if err != nil {
		return err
	}
	toolchain := strings.TrimSpace(string(version))
	if !strings.HasPrefix(toolchain, "go1.") {
		return fmt.Errorf("unsupported Go toolchain %q", toolchain)
	}
	// A local replacement could compile bytes outside the captured tree. Remote
	// replacements remain described by go.mod/go.sum and module verification.
	mod, err := commandOutput(ctx, source, env, "mod", "edit", "-json")
	if err != nil {
		return err
	}
	var module struct {
		Replace []struct {
			New struct{ Path, Version string }
		}
	}
	if err = json.Unmarshal(mod, &module); err != nil {
		return err
	}
	for _, replacement := range module.Replace {
		if replacement.New.Version == "" {
			return fmt.Errorf("local module replacements are not supported by bundle builds")
		}
	}
	id := identity(files, toolchain, label)
	stamp, err := buildmeta.Encode(id, label)
	if err != nil {
		return err
	}
	outputs := targets()
	for _, target := range outputs {
		path := filepath.Join(scratch, target.name)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-mod=readonly", "-ldflags=-X=github.com/nchapman/vamos/internal/buildmeta.Stamp="+stamp, "-o", path, "./cmd/vamos")
		cmd.Dir = source
		cmd.Env = append(append([]string{}, env...), "GOOS="+target.os, "GOARCH="+target.arch)
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("build %s: %w", target.name, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err = buildmeta.Check(stamp, data); err != nil {
			return fmt.Errorf("verify %s: %w", target.name, err)
		}
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return err
		}
		if info.GoVersion != toolchain {
			return fmt.Errorf("Go toolchain changed during bundle build")
		}
	}
	if err = os.MkdirAll(output, 0755); err != nil {
		return err
	}
	// Copy into the output filesystem before renaming; scratch may be on another
	// volume. Do not truncate an executable another process could be reading.
	for _, target := range outputs {
		if err = publish(filepath.Join(scratch, target.name), filepath.Join(output, target.name)); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "Built %s bundle %s\n", label, id)
	return nil
}

func commandOutput(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// Hash/copy all files under cmd and internal conservatively, including embedded
// assets and tests, plus module files. No Git metadata or timestamps are used.
func snapshot(root, dest string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	for _, base := range []string{"cmd", "internal", "go.mod", "go.sum"} {
		err := filepath.WalkDir(filepath.Join(root, base), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("bundle input is not a regular file: %s", path)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = data
			out := filepath.Join(dest, rel)
			if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
				return err
			}
			return os.WriteFile(out, data, 0600)
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func identity(files map[string][]byte, toolchain, label string) string {
	hash := sha256.New()
	record := func(name string, data []byte) {
		fmt.Fprintf(hash, "%d:%s%d:", len(name), name, len(data))
		hash.Write(data)
	}
	record("profile", []byte(profile))
	record("toolchain", []byte(toolchain))
	record("version", []byte(label))
	keys := make([]string, 0, len(files))
	for name := range files {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		record(name, files[name])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func buildEnvironment(in []string) []string {
	var out []string
	// GOENV=off disables persistent go env -w settings. Keep module proxy/cache
	// configuration but exclude flags, workspaces, tuning, instrumentation, and
	// compiler selection inherited from the developer shell.
	fixed := map[string]string{
		"GOENV":        "off",
		"GOWORK":       "off",
		"GOFLAGS":      "",
		"GOTOOLCHAIN":  "local",
		"GOEXPERIMENT": "",
		"GO111MODULE":  "on",
		"GOFIPS140":    "off",
		"CGO_ENABLED":  "0",
		"GOAMD64":      "v1",
		"GOARM64":      "v8.0",
	}
	for _, entry := range in {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GOOS" || upper == "GOARCH" {
			continue
		}
		if _, ok := fixed[upper]; ok {
			continue
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(fixed))
	for key := range fixed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+fixed[key])
	}
	return out
}

func publish(source, dest string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".vamos-build-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = io.Copy(temp, bytes.NewReader(data)); err != nil {
		_ = temp.Close()
		return err
	}
	if err = temp.Chmod(0755); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), dest); err != nil {
		if runtime.GOOS == "windows" {
			if rmErr := os.Remove(dest); rmErr == nil || errors.Is(rmErr, os.ErrNotExist) {
				return os.Rename(temp.Name(), dest)
			}
		}
		return err
	}
	return nil
}
