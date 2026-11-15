package backupbackend

import (
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Artifact struct{ Architecture, URL, CompressedSHA256, SHA256 string }

// Artifacts returns independent value copies, not mutable global pin state.
func Artifacts() []Artifact {
	return []Artifact{
		{"amd64", "https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2", "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c", "20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639"},
		{"arm64", "https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_arm64.bz2", "a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465", "2fb45ac6f9071b6f20eb883953a188f9e7c7cb6bbe43c67a2e47ada4e85ee7f0"},
	}
}
func safeDirectory(path string, create bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		st, e := os.Lstat(current)
		if os.IsNotExist(e) && create {
			if os.Mkdir(current, 0755) != nil {
				return false
			}
			st, e = os.Lstat(current)
		}
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 || !owned(st) {
			return false
		}
	}
	return true
}
func verifyFile(path, hash string) bool {
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0755 || !owned(st) || st.Size() > 64<<20 {
		return false
	}
	f, e := os.Open(path)
	if e != nil {
		return false
	}
	defer f.Close()
	after, e := f.Stat()
	if e != nil || !os.SameFile(st, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0755 || !owned(after) || after.Size() > 64<<20 {
		return false
	}
	h := sha256.New()
	if _, e = io.Copy(h, io.LimitReader(f, 64<<20+1)); e != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == hash
}
func (b *Backend) ensure(ctx context.Context, scratch string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errTool
	}
	var a Artifact
	for _, v := range Artifacts() {
		if v.Architecture == runtime.GOARCH {
			a = v
		}
	}
	if a.URL == "" {
		return "", errTool
	}
	if !safeDirectory(b.root, false) {
		return "", errTool
	}
	dir := filepath.Join(b.root, "usr/local/lib/vamos/tools/restic", Version)
	if !safeDirectory(dir, true) {
		return "", errTool
	}
	dest := filepath.Join(dir, "restic")
	if _, e := os.Lstat(dest); e == nil {
		if !verifyFile(dest, a.SHA256) {
			return "", errTool
		}
		return dest, nil
	} else if !os.IsNotExist(e) {
		return "", errTool
	}
	installed, e := installArtifact(ctx, dir, scratch, a)
	if e != nil || !verifyFile(installed, a.SHA256) {
		return "", errTool
	}
	return installed, nil
}

// installArtifact requires a verified version directory from ensure. Runtime
// scratch is deliberately unused: installation must not depend on its filesystem.
func installArtifact(ctx context.Context, dir, _ string, a Artifact) (string, error) {
	// Only public release payloads belong here. Credentials remain in runtime
	// scratch, which may reside on a different filesystem (for example /run).
	scratch, e := os.MkdirTemp(dir, ".install-")
	if e != nil {
		return "", errTool
	}
	defer os.RemoveAll(scratch)
	dest := filepath.Join(dir, "restic")
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if e != nil {
		return "", errTool
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 10 {
			return errTool
		}
		return nil
	}}
	resp, e := client.Do(req)
	if e != nil {
		return "", errTool
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errTool
	}
	compressed, e := os.OpenFile(filepath.Join(scratch, "download"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return "", errTool
	}
	defer compressed.Close()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(compressed, h), io.LimitReader(resp.Body, 32<<20+1))
	if e != nil || n > 32<<20 || hex.EncodeToString(h.Sum(nil)) != a.CompressedSHA256 {
		return "", errTool
	}
	if _, e = compressed.Seek(0, 0); e != nil {
		return "", errTool
	}
	f, e := os.OpenFile(filepath.Join(scratch, "binary"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return "", errTool
	}
	binaryName := f.Name()
	h.Reset()
	n, e = io.Copy(io.MultiWriter(f, h), io.LimitReader(bzip2.NewReader(compressed), 64<<20+1))
	if e != nil || n > 64<<20 || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		_ = f.Close()
		return "", errTool
	}
	if f.Chmod(0755) != nil || f.Sync() != nil {
		_ = f.Close()
		return "", errTool
	}
	if f.Close() != nil {
		return "", errTool
	}
	// Staging within the version directory keeps this atomic, no-overwrite
	// hard link on the destination filesystem, independent of runtime storage.
	if os.Link(binaryName, dest) != nil {
		return "", errTool
	}
	directory, e := os.Open(dir)
	if e != nil {
		return "", errTool
	}
	e = directory.Sync()
	closeErr := directory.Close()
	if e != nil || closeErr != nil {
		return "", errTool
	}
	return dest, nil
}
