package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	archiveHash := hex.EncodeToString(sum[:])
	sumsTxt := fmt.Sprintf("%s  %s\n", archiveHash, archiveName)
	sumsHash := sha256Hex([]byte(sumsTxt))
	attestationJSON, roots := buildTestAttestation(t, "0.2.0", archiveName, sumsHash, archiveHash)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v0.2.0"}`))
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		case "/attestations/sha256:" + sumsHash:
			_, _ = w.Write(attestationJSON)
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
		fulcioRoots:  roots,
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
	archiveHash := hex.EncodeToString(sum[:])
	sumsTxt := fmt.Sprintf("%s  %s\n", archiveHash, archiveName)
	sumsHash := sha256Hex([]byte(sumsTxt))
	attestationJSON, roots := buildTestAttestation(t, "0.2.0", archiveName, sumsHash, archiveHash)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		case "/attestations/sha256:" + sumsHash:
			_, _ = w.Write(attestationJSON)
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
		fulcioRoots:  roots,
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

func TestUpgradeRejectsMissingOrInvalidAttestation(t *testing.T) {
	installDir := t.TempDir()
	exePath := filepath.Join(installDir, "vamos")
	if err := os.WriteFile(exePath, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	archiveName := "vamos_0.2.0_linux_amd64.tar.gz"
	archiveBytes := buildTestTarGz(t, map[string]string{
		"vamos_0.2.0_linux_amd64/vamos":             "new-cli",
		"vamos_0.2.0_linux_amd64/vamos-linux-amd64": "new-amd64",
		"vamos_0.2.0_linux_amd64/vamos-linux-arm64": "new-arm64",
	})
	sum := sha256.Sum256(archiveBytes)
	archiveHash := hex.EncodeToString(sum[:])
	sumsTxt := fmt.Sprintf("%s  %s\n", archiveHash, archiveName)
	sumsHash := sha256Hex([]byte(sumsTxt))
	// Attestation signed for a different version tag (0.1.9 instead of 0.2.0) must be rejected.
	wrongTagAttestation, roots := buildTestAttestation(t, "0.1.9", archiveName, sumsHash, archiveHash)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/v0.2.0/SHA256SUMS":
			_, _ = w.Write([]byte(sumsTxt))
		case "/download/v0.2.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		case "/attestations/sha256:" + sumsHash:
			if wrongTagAttestation == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(wrongTagAttestation)
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
		fulcioRoots:  roots,
	}

	var out, errOut bytes.Buffer
	if code := upgradeCommand(context.Background(), []string{"--version", "0.2.0"}, &out, &errOut, "0.1.0", deps); code != 1 {
		t.Fatalf("code=%d, want 1; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "build provenance verification failed") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	got, err := os.ReadFile(exePath)
	if err != nil || string(got) != "keep-me" {
		t.Fatalf("existing binary modified: got=%q err=%v", got, err)
	}

	// Missing attestation (404) must also fail closed without touching the existing binary.
	wrongTagAttestation = nil
	out.Reset()
	errOut.Reset()
	if code := upgradeCommand(context.Background(), []string{"--version", "0.2.0"}, &out, &errOut, "0.1.0", deps); code != 1 {
		t.Fatalf("code=%d, want 1; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "build provenance attestation") {
		t.Fatalf("stderr=%q", errOut.String())
	}
}

func TestRealSigstoreFulcioAttestationBundleVerifies(t *testing.T) {
	roots, intermediates, err := defaultFulcioPools()
	if err != nil {
		t.Fatal(err)
	}
	bundle := sigstoreBundle{}
	bundle.VerificationMaterial.Certificate.RawBytes = "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"
	bundle.DSSEEnvelope.PayloadType = inTotoPayloadType
	bundle.DSSEEnvelope.Payload = "eyJfdHlwZSI6Imh0dHBzOi8vaW4tdG90by5pby9TdGF0ZW1lbnQvdjEiLCJzdWJqZWN0IjpbeyJuYW1lIjoiU0hBMjU2U1VNUyIsImRpZ2VzdCI6eyJzaGEyNTYiOiI1MTE0YWM4ZTc0MjQ2ODgwMGU4MWM2MGE4ZTAyY2JiN2U4ZjY2ZTVkYzdiYWJhN2EyOWU4ZTJjMjk4YjRjNzQwIn19LHsibmFtZSI6InZhbW9zXzAuMy4wX2Rhcndpbl9hbWQ2NC50YXIuZ3oiLCJkaWdlc3QiOnsic2hhMjU2IjoiOThjYTgwNTFkODY3NmYxOTZhY2JjNjgzODU0OGYzMzQ5OTE5MjMwNDU4ZTc0MjMzZGQ4NTE3N2RhMDVjMzQyZCJ9fSx7Im5hbWUiOiJ2YW1vc18wLjMuMF9kYXJ3aW5fYXJtNjQudGFyLmd6IiwiZGlnZXN0Ijp7InNoYTI1NiI6IjRjNzhiZTIzYTRhOTU2NGUwNDUxNDU1MWQ1ZTNlOTRiZjFhMWJlNjk0NjdjZWMzY2Y4NzJkMjhhNTE2YjI4ZjAifX0seyJuYW1lIjoidmFtb3NfMC4zLjBfbGludXhfYW1kNjQudGFyLmd6IiwiZGlnZXN0Ijp7InNoYTI1NiI6IjQ0MjJlNTM4NTNkYjZkZGQ1YTIwMzVlYTNmMzA4ODAyMTE1MDE2MjAzODc4YTljZDY0ZjNiNzZhMGZhNmIwMDkifX0seyJuYW1lIjoidmFtb3NfMC4zLjBfbGludXhfYXJtNjQudGFyLmd6IiwiZGlnZXN0Ijp7InNoYTI1NiI6Ijk2YTc4YTg1YmQxM2FlNTAzYjYwMmViMGRjZjhlZjYwMGRhMTY0MDlkNzY3MWRlNmRjZDQxMzI4Zjk4YTU5NjAifX0seyJuYW1lIjoidmFtb3NfMC4zLjBfd2luZG93c19hbWQ2NC56aXAiLCJkaWdlc3QiOnsic2hhMjU2IjoiYjY1M2MwMWU0MTlhYzBlYjRlYTBmMTllNDhjZDk1ZDE5ZTFmMDY0M2QzMmQ5OWI0NGQ0M2Q4ZTE5YWIzYTBiYiJ9fV0sInByZWRpY2F0ZVR5cGUiOiJodHRwczovL3Nsc2EuZGV2L3Byb3ZlbmFuY2UvdjEiLCJwcmVkaWNhdGUiOnsiYnVpbGREZWZpbml0aW9uIjp7ImJ1aWxkVHlwZSI6Imh0dHBzOi8vYWN0aW9ucy5naXRodWIuaW8vYnVpbGR0eXBlcy93b3JrZmxvdy92MSIsImV4dGVybmFsUGFyYW1ldGVycyI6eyJ3b3JrZmxvdyI6eyJyZWYiOiJyZWZzL3RhZ3MvdjAuMy4wIiwicmVwb3NpdG9yeSI6Imh0dHBzOi8vZ2l0aHViLmNvbS9uY2hhcG1hbi92YW1vcyIsInBhdGgiOiIuZ2l0aHViL3dvcmtmbG93cy9yZWxlYXNlLnltbCJ9fSwiaW50ZXJuYWxQYXJhbWV0ZXJzIjp7ImdpdGh1YiI6eyJldmVudF9uYW1lIjoicHVzaCIsInJlcG9zaXRvcnlfaWQiOiIxNDU0NzkyNzg2IiwicmVwb3NpdG9yeV9vd25lcl9pZCI6IjMwOTUiLCJydW5uZXJfZW52aXJvbm1lbnQiOiJzZWxmLWhvc3RlZCJ9fSwicmVzb2x2ZWREZXBlbmRlbmNpZXMiOlt7InVyaSI6ImdpdCtodHRwczovL2dpdGh1Yi5jb20vbmNoYXBtYW4vdmFtb3NAcmVmcy90YWdzL3YwLjMuMCIsImRpZ2VzdCI6eyJnaXRDb21taXQiOiIzN2UwMjgzYjFlYjU4OTYzNDFjYjJmNDIzNjIwYmRlMWE5ZWM2MzFmIn19XX0sInJ1bkRldGFpbHMiOnsiYnVpbGRlciI6eyJpZCI6Imh0dHBzOi8vZ2l0aHViLmNvbS9uY2hhcG1hbi92YW1vcy8uZ2l0aHViL3dvcmtmbG93cy9yZWxlYXNlLnltbEByZWZzL3RhZ3MvdjAuMy4wIn0sIm1ldGFkYXRhIjp7Imludm9jYXRpb25JZCI6Imh0dHBzOi8vZ2l0aHViLmNvbS9uY2hhcG1hbi92YW1vcy9hY3Rpb25zL3J1bnMvNDQxNDQ0Mzc4MjYvYXR0ZW1wdHMvMSJ9fX19"
	bundle.DSSEEnvelope.Signatures = []struct {
		Sig string `json:"sig"`
	}{{Sig: "MEQCIHbEm1ur4g+vFIzqaYjqVBOTjYU24P0Oqu+Zy2cq8wjMAiBFSC/yrXMN/+Iihcsc/fjl2CqeN06sRsOC7QPd5id5rQ=="}}

	const (
		sumsHash    = "5114ac8e742468800e81c60a8e02cbb7e8f66e5dc7baba7a29e8e2c298b4c740"
		archiveName = "vamos_0.3.0_darwin_arm64.tar.gz"
		archiveHash = "4c78be23a4a9564e04514551d5e3e94bf1a1be69467cec3cf872d28a516b28f0"
	)
	if err := verifyAttestationBundle(bundle, "0.3.0", archiveName, sumsHash, archiveHash, roots, intermediates); err != nil {
		t.Fatalf("real Sigstore Fulcio bundle failed verification: %v", err)
	}
	if err := verifyAttestationBundle(bundle, "0.3.1", archiveName, sumsHash, archiveHash, roots, intermediates); err == nil {
		t.Fatal("accepted wrong version tag")
	}
	if err := verifyAttestationBundle(bundle, "0.3.0", archiveName, sumsHash, strings.Repeat("0", 64), roots, intermediates); err == nil {
		t.Fatal("accepted mismatched archive digest")
	}
}

func buildTestAttestation(t *testing.T, version, archiveName, sumsHash, archiveHash string) ([]byte, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"sigstore.dev"}, CommonName: "test-fulcio-root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sanURI, err := url.Parse(fmt.Sprintf("%s/%s@refs/tags/v%s", expectedRepoURL, expectedWorkflowPath, version))
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "fulcio-leaf"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(5 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{sanURI},
		ExtraExtensions: []pkix.Extension{{
			Id:    oidFulcioIssuerV1,
			Value: []byte(expectedOIDCIssuer),
		}},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	var stmt inTotoStatement
	stmt.Type = inTotoStatementType
	stmt.PredicateType = slsaPredicateType
	stmt.Subject = []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	}{
		{Name: "SHA256SUMS", Digest: map[string]string{"sha256": sumsHash}},
		{Name: archiveName, Digest: map[string]string{"sha256": archiveHash}},
	}
	stmt.Predicate.BuildDefinition.ExternalParameters.Workflow.Repository = expectedRepoURL
	stmt.Predicate.BuildDefinition.ExternalParameters.Workflow.Path = expectedWorkflowPath
	stmt.Predicate.BuildDefinition.ExternalParameters.Workflow.Ref = "refs/tags/v" + version

	payloadBytes, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	pae := dssePAE(inTotoPayloadType, payloadBytes)
	digest := sha256.Sum256(pae)
	sig, err := ecdsa.SignASN1(rand.Reader, leafKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	var resp attestationResponse
	var b sigstoreBundle
	b.VerificationMaterial.Certificate.RawBytes = base64.StdEncoding.EncodeToString(leafDER)
	b.DSSEEnvelope.PayloadType = inTotoPayloadType
	b.DSSEEnvelope.Payload = base64.StdEncoding.EncodeToString(payloadBytes)
	b.DSSEEnvelope.Signatures = []struct {
		Sig string `json:"sig"`
	}{{Sig: base64.StdEncoding.EncodeToString(sig)}}
	resp.Attestations = append(resp.Attestations, struct {
		Bundle sigstoreBundle `json:"bundle"`
	}{Bundle: b})

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return raw, roots
}
