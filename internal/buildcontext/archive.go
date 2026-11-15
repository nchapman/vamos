// Package buildcontext constructs bounded, uncompressed Docker build contexts.
package buildcontext

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/nchapman/vamos/internal/config"
)

const MaxBytes = 32 * 1024 * 1024

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxBytes-b.Len() {
		return 0, fmt.Errorf("build context exceeds %d byte limit", MaxBytes)
	}
	return b.Buffer.Write(p)
}

func reserved(name string) bool {
	for _, p := range strings.Split(filepath.ToSlash(name), "/") {
		if p == ".git" || p == ".vamos" {
			return true
		}
	}
	return false
}

// Archive resolves Context relative to configFile's directory. It does not follow
// directory symlinks. Dockerfile and the selected ignore file are materialized as
// regular files, so exclusions cannot make either unusable in the remote build.
// All other included symlinks must resolve within the context and use relative
// targets. Files excluded by Docker ignore rules are never opened.
func Archive(configFile string, build config.Build) ([]byte, error) {
	context := build.Context
	if context == "" {
		context = "."
	}
	if !filepath.IsAbs(context) {
		context = filepath.Join(filepath.Dir(configFile), context)
	}
	rootPath, err := filepath.Abs(context)
	if err != nil {
		return nil, err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dockerfile := build.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	// Also reject portable Windows escape spellings on Unix hosts.
	if !filepath.IsLocal(dockerfile) || strings.ContainsAny(dockerfile, "\\:") {
		return nil, fmt.Errorf("unsafe Dockerfile path %q", dockerfile)
	}
	dockerfile = filepath.Clean(dockerfile)
	if reserved(dockerfile) {
		return nil, fmt.Errorf("Dockerfile is in a reserved directory")
	}
	validate := func(name string) error {
		resolved, e := filepath.EvalSymlinks(filepath.Join(rootPath, name))
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(rootPath, resolved)
		if e != nil || !filepath.IsLocal(rel) || reserved(rel) {
			return fmt.Errorf("unsafe path %q: escapes context or enters reserved directory", name)
		}
		return nil
	}
	if err = validate(dockerfile); err != nil {
		return nil, fmt.Errorf("Dockerfile: %w", err)
	}
	info, err := root.Stat(dockerfile)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Dockerfile must be a regular file")
	}
	selected := ""
	for _, candidate := range []string{dockerfile + ".dockerignore", ".dockerignore"} {
		_, e := root.Lstat(candidate)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		selected = candidate
		break
	}
	var patterns []string
	if selected != "" {
		if err = validate(selected); err != nil {
			return nil, err
		}
		st, e := root.Stat(selected)
		if e != nil {
			return nil, e
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("ignore file must be regular")
		}
		if st.Size() > MaxBytes {
			return nil, fmt.Errorf("ignore file exceeds %d byte limit", MaxBytes)
		}
		f, e := root.Open(selected)
		if e != nil {
			return nil, e
		}
		// Bound reads even if the file grows after Stat.
		data, e := io.ReadAll(io.LimitReader(f, MaxBytes+1))
		f.Close()
		if e != nil {
			return nil, e
		}
		if len(data) > MaxBytes {
			return nil, fmt.Errorf("ignore file exceeds %d byte limit", MaxBytes)
		}
		patterns, err = ignorefile.ReadAll(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, err
	}
	var out boundedBuffer
	tw := tar.NewWriter(&out)
	add := func(name string, st fs.FileInfo, link string) error {
		if st.Size() > MaxBytes && st.Mode().IsRegular() {
			return fmt.Errorf("build context exceeds %d byte limit", MaxBytes)
		}
		h, e := tar.FileInfoHeader(st, link)
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(name)
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if st.Mode().IsRegular() {
			f, e := root.Open(name)
			if e != nil {
				return e
			}
			defer f.Close()
			current, e := f.Stat()
			if e != nil {
				return e
			}
			if !current.Mode().IsRegular() || !os.SameFile(st, current) {
				return fmt.Errorf("context file changed: %s", name)
			}
			_, e = io.CopyN(tw, f, st.Size())
			return e
		}
		return nil
	}
	for _, name := range []string{dockerfile, selected} {
		if name == "" {
			continue
		}
		st, e := root.Stat(name)
		if e != nil {
			return nil, e
		}
		if e = add(name, st, ""); e != nil {
			return nil, e
		}
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if reserved(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if name == filepath.ToSlash(dockerfile) || name == filepath.ToSlash(selected) {
			return nil
		}
		excluded, e := matcher.MatchesOrParentMatches(name)
		if e != nil {
			return e
		}
		// Do not prune ignored directories: a later negation may include descendants.
		if excluded {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		link := ""
		if st.Mode()&os.ModeSymlink != 0 {
			link, e = root.Readlink(name)
			if e != nil {
				return e
			}
			if filepath.IsAbs(link) || strings.ContainsAny(link, "\\:") {
				return fmt.Errorf("unsafe symlink %q", name)
			}
			if e = validate(name); e != nil {
				return fmt.Errorf("unsafe symlink %q: %w", name, e)
			}
			link = filepath.ToSlash(link)
		} else if !st.IsDir() && !st.Mode().IsRegular() {
			return fmt.Errorf("unsupported special file %q", name)
		}
		return add(name, st, link)
	})
	if err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
