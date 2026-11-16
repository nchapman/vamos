package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/registry"
)

func TestUpgradeHelpSupported(t *testing.T) {
	for _, args := range [][]string{{"upgrade", "--help"}, {"help", "upgrade"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut, "0.1.0"); code != 0 || errOut.Len() != 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, errOut.String())
		}
		if !strings.Contains(out.String(), "Usage: vamos upgrade") {
			t.Fatalf("args=%v unexpected help: %q", args, out.String())
		}
	}
}

func TestUpgradeCheckReportsAvailableAndUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.0"}`))
	}))
	defer srv.Close()

	deps := upgradeDeps{
		httpClient: srv.Client(),
		apiBase:    srv.URL,
	}

	var out, errOut bytes.Buffer
	if code := upgradeCommand(context.Background(), []string{"--check"}, &out, &errOut, "0.1.0", deps); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Upgrade available: 0.1.0 -> v0.2.0") {
		t.Fatalf("stdout=%q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := upgradeCommand(context.Background(), []string{"--check"}, &out, &errOut, "0.2.0", deps); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestUpgradeInstallsTarGzBundleAndResolvesSymlink(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "real-bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(installDir, "vamos")
	if err := os.WriteFile(realExe, []byte("old-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := realExe
	if runtime.GOOS != "windows" {
		linkDir := filepath.Join(t.TempDir(), "link-bin")
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		symlinkPath = filepath.Join(linkDir, "vamos")
		if err := os.Symlink(realExe, symlinkPath); err != nil {
			t.Fatal(err)
		}
	}

	archiveName := "vamos_0.2.0_darwin_arm64.tar.gz"
	archiveBytes := buildTestTarGz(t, map[string]string{
		"vamos_0.2.0_darwin_arm64/vamos":             "new-cli",
		"vamos_0.2.0_darwin_arm64/vamos-linux-amd64": "new-amd64",
		"vamos_0.2.0_darwin_arm64/vamos-linux-arm64": "new-arm64",
		"vamos_0.2.0_darwin_arm64/README.md":         "ignored",
	})
	sum := sha256.Sum256(archiveBytes)
	sumsTxt := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v0.2.0"}`))
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	regDir := t.TempDir()
	regStore := registry.New(regDir)
	if err := regStore.Put(registry.Server{Name: "prod", Target: "root@example.com", Domain: "apps.example.com"}); err != nil {
		t.Fatal(err)
	}

	deps := upgradeDeps{
		httpClient:   srv.Client(),
		apiBase:      srv.URL,
		downloadBase: srv.URL + "/download",
		goos:         "darwin",
		goarch:       "arm64",
		executable:   func() (string, error) { return symlinkPath, nil },
		openRegistry: func() (*registry.Store, error) { return regStore, nil },
	}

	var out, errOut bytes.Buffer
	if code := upgradeCommand(context.Background(), nil, &out, &errOut, "0.1.0", deps); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Upgraded vamos to v0.2.0") {
		t.Fatalf("stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "vamos server upgrade --yes prod") {
		t.Fatalf("expected server upgrade hint for prod, got stdout=%q", out.String())
	}

	for name, want := range map[string]string{
		"vamos":             "new-cli",
		"vamos-linux-amd64": "new-amd64",
		"vamos-linux-arm64": "new-arm64",
	} {
		path := filepath.Join(installDir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", name, err)
			}
			execBit := info.Mode().Perm()&0o111 != 0
			wantExec := name == "vamos"
			if execBit != wantExec {
				t.Fatalf("%s mode=%o executable=%v, want %v", name, info.Mode().Perm(), execBit, wantExec)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(installDir, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("README.md should not be installed into bin dir, got err=%v", err)
	}
}

func TestUpgradeInstallsZipBundle(t *testing.T) {
	installDir := t.TempDir()
	archiveName := "vamos_0.2.0_windows_amd64.zip"
	archiveBytes := buildTestZip(t, map[string]string{
		"vamos_0.2.0_windows_amd64/vamos.exe":         "new-exe",
		"vamos_0.2.0_windows_amd64/vamos-linux-amd64": "new-amd64",
		"vamos_0.2.0_windows_amd64/vamos-linux-arm64": "new-arm64",
	})
	sum := sha256.Sum256(archiveBytes)
	sumsTxt := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	deps := upgradeDeps{
		httpClient:   srv.Client(),
		apiBase:      srv.URL,
		downloadBase: srv.URL + "/download",
		goos:         "windows",
		goarch:       "amd64",
		executable:   func() (string, error) { return filepath.Join(installDir, "vamos.exe"), nil },
	}

	var out, errOut bytes.Buffer
	if code := upgradeCommand(context.Background(), []string{"--version", "v0.2.0"}, &out, &errOut, "0.1.0", deps); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	for name, want := range map[string]string{
		"vamos.exe":         "new-exe",
		"vamos-linux-amd64": "new-amd64",
		"vamos-linux-arm64": "new-arm64",
	} {
		got, err := os.ReadFile(filepath.Join(installDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUpgradeRejectsChecksumMismatch(t *testing.T) {
	installDir := t.TempDir()
	exePath := filepath.Join(installDir, "vamos")
	if err := os.WriteFile(exePath, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}

	archiveName := "vamos_0.2.0_linux_amd64.tar.gz"
	archiveBytes := buildTestTarGz(t, map[string]string{
		"vamos_0.2.0_linux_amd64/vamos":             "tampered",
		"vamos_0.2.0_linux_amd64/vamos-linux-amd64": "tampered",
		"vamos_0.2.0_linux_amd64/vamos-linux-arm64": "tampered",
	})
	sumsTxt := fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	deps := upgradeDeps{
		httpClient:   srv.Client(),
		apiBase:      srv.URL,
		downloadBase: srv.URL + "/download",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return exePath, nil },
	}

	var out, errOut bytes.Buffer
	if code := upgradeCommand(context.Background(), []string{"--version", "0.2.0"}, &out, &errOut, "0.1.0", deps); code != 1 {
		t.Fatalf("code=%d, want 1; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "checksum mismatch") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	got, err := os.ReadFile(exePath)
	if err != nil || string(got) != "keep-me" {
		t.Fatalf("existing binary modified: got=%q err=%v", got, err)
	}
}

func buildTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
