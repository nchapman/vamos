package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/registry"
)

const (
	defaultReleaseAPIBase      = "https://api.github.com/repos/nchapman/vamos"
	defaultReleaseDownloadBase = "https://github.com/nchapman/vamos/releases/download"
	maxChecksumsBytes          = 1 << 20
	maxBundleBytes             = 256 << 20
	maxBinaryBytes             = 128 << 20
)

type upgradeDeps struct {
	httpClient   *http.Client
	apiBase      string
	downloadBase string
	goos         string
	goarch       string
	executable   func() (string, error)
	openRegistry func() (*registry.Store, error)
}

func defaultUpgradeDeps() upgradeDeps {
	return upgradeDeps{
		httpClient:   &http.Client{Timeout: 2 * time.Minute},
		apiBase:      defaultReleaseAPIBase,
		downloadBase: defaultReleaseDownloadBase,
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
		executable:   os.Executable,
		openRegistry: registry.Default,
	}
}

func upgradeCommand(ctx context.Context, args []string, stdout, stderr io.Writer, currentVersion string, deps upgradeDeps) int {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkOnly := flags.Bool("check", false, "Check whether a newer release is available without installing")
	targetVer := flags.String("version", "", "Install a specific release version (for example 0.2.0 or v0.2.0)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: vamos upgrade [--check] [--version VERSION]")
		fmt.Fprintln(flags.Output(), "Upgrade the vamos CLI and its sibling Linux executor binaries from GitHub Releases.")
		flags.PrintDefaults()
	}
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "upgrade does not accept positional arguments")
	}

	explicitVersion := strings.TrimPrefix(strings.TrimSpace(*targetVer), "v")
	version := explicitVersion
	if version == "" {
		latest, err := fetchLatestVersion(ctx, deps, currentVersion)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		version = latest
	}

	current := strings.TrimPrefix(strings.TrimSpace(currentVersion), "v")
	if explicitVersion == "" && current != "" && current != "dev" && current == version {
		fmt.Fprintf(stdout, "vamos is already up to date (%s)\n", currentVersion)
		return 0
	}

	if *checkOnly {
		if current == version {
			fmt.Fprintf(stdout, "vamos is already up to date (%s)\n", currentVersion)
		} else {
			fmt.Fprintf(stdout, "Upgrade available: %s -> v%s\n", currentVersion, version)
		}
		return 0
	}

	installDir, err := resolveInstallDir(deps.executable)
	if err != nil {
		return lifecycleError(stderr, err)
	}

	if err := installReleaseBundle(ctx, deps, currentVersion, version, installDir); err != nil {
		return lifecycleError(stderr, err)
	}

	fmt.Fprintf(stdout, "Upgraded vamos to v%s in %s\n", version, installDir)
	writeServerUpgradeHint(stdout, deps.openRegistry)
	return 0
}

func writeServerUpgradeHint(w io.Writer, openRegistry func() (*registry.Store, error)) {
	if openRegistry == nil {
		return
	}
	store, err := openRegistry()
	if err != nil {
		return
	}
	state, err := store.Read()
	if err != nil || len(state.Servers) == 0 {
		return
	}
	names := make([]string, 0, len(state.Servers))
	for name := range state.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 1 {
		fmt.Fprintf(w, "To upgrade your registered server: vamos server upgrade --yes %s\n", names[0])
		return
	}
	fmt.Fprintf(w, "To upgrade a registered server (%s): vamos server upgrade --yes <name>\n", strings.Join(names, ", "))
}

func fetchLatestVersion(ctx context.Context, deps upgradeDeps, currentVersion string) (string, error) {
	url := strings.TrimRight(deps.apiBase, "/") + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vamos/"+currentVersion)

	resp, err := deps.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check latest release: GitHub API returned %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChecksumsBytes)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v")
	if version == "" {
		return "", errors.New("latest release tag is empty")
	}
	return version, nil
}

func resolveInstallDir(executable func() (string, error)) (string, error) {
	self, err := executable()
	if err != nil {
		return "", fmt.Errorf("locate current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return filepath.Dir(self), nil
}

func bundleArchiveName(version, goos, goarch string) (string, error) {
	switch goos {
	case "darwin", "linux", "windows":
	default:
		return "", fmt.Errorf("unsupported OS %q for self-update", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture %q for self-update", goarch)
	}
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("vamos_%s_%s_%s.%s", version, goos, goarch, ext), nil
}

func installReleaseBundle(ctx context.Context, deps upgradeDeps, currentVersion, version, installDir string) error {
	archiveName, err := bundleArchiveName(version, deps.goos, deps.goarch)
	if err != nil {
		return err
	}

	baseURL := strings.TrimRight(deps.downloadBase, "/") + "/v" + version
	sumsBytes, err := downloadBytes(ctx, deps, currentVersion, baseURL+"/SHA256SUMS", maxChecksumsBytes)
	if err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	expectedHash, err := lookupChecksum(sumsBytes, archiveName)
	if err != nil {
		return err
	}

	archiveBytes, err := downloadBytes(ctx, deps, currentVersion, baseURL+"/"+archiveName, maxBundleBytes)
	if err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}
	actualSum := sha256.Sum256(archiveBytes)
	if !strings.EqualFold(hex.EncodeToString(actualSum[:]), expectedHash) {
		return fmt.Errorf("checksum mismatch for %s", archiveName)
	}

	cliName := "vamos"
	if deps.goos == "windows" {
		cliName = "vamos.exe"
	}
	expectedFiles := map[string]bool{
		cliName:             true,
		"vamos-linux-amd64": true,
		"vamos-linux-arm64": true,
	}

	binaries, err := extractReleaseBinaries(archiveName, archiveBytes, expectedFiles)
	if err != nil {
		return err
	}
	for name := range expectedFiles {
		if _, ok := binaries[name]; !ok {
			return fmt.Errorf("release archive %s is missing %s", archiveName, name)
		}
	}

	orderedNames := []string{"vamos-linux-amd64", "vamos-linux-arm64", cliName}
	return commitStagedBinaries(installDir, orderedNames, binaries)
}

func downloadBytes(ctx context.Context, deps upgradeDeps, currentVersion, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vamos/"+currentVersion)

	resp, err := deps.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func lookupChecksum(sums []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if fields[1] == filename {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum for %s not found in SHA256SUMS", filename)
}

func extractReleaseBinaries(archiveName string, data []byte, expected map[string]bool) (map[string][]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractZipBinaries(data, expected)
	}
	return extractTarGzBinaries(data, expected)
}

func extractTarGzBinaries(data []byte, expected map[string]bool) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer gz.Close()

	out := make(map[string][]byte, len(expected))
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if !expected[base] {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxBinaryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", base, err)
		}
		if len(body) > maxBinaryBytes {
			return nil, fmt.Errorf("extracted %s exceeds size limit", base)
		}
		out[base] = body
	}
	return out, nil
}

func extractZipBinaries(data []byte, expected map[string]bool) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open release zip: %w", err)
	}
	out := make(map[string][]byte, len(expected))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if !expected[base] {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", base, err)
		}
		body, err := io.ReadAll(io.LimitReader(rc, maxBinaryBytes+1))
		closeErr := rc.Close()
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", base, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", base, closeErr)
		}
		if len(body) > maxBinaryBytes {
			return nil, fmt.Errorf("extracted %s exceeds size limit", base)
		}
		out[base] = body
	}
	return out, nil
}

type stagedBinary struct {
	dest   string
	tmp    string
	backup string
	hadOld bool
}

func commitStagedBinaries(installDir string, orderedNames []string, binaries map[string][]byte) error {
	staged := make([]stagedBinary, 0, len(orderedNames))
	defer func() {
		for _, s := range staged {
			_ = os.Remove(s.tmp)
		}
	}()

	for _, name := range orderedNames {
		dest := filepath.Join(installDir, name)
		mode := os.FileMode(0o755)
		if strings.HasPrefix(name, "vamos-linux-") {
			mode = 0o644
		}
		tmpPath, err := stageTempBinary(installDir, binaries[name], mode)
		if err != nil {
			return fmt.Errorf("stage %s: %w", dest, err)
		}
		staged = append(staged, stagedBinary{dest: dest, tmp: tmpPath})
	}

	committed := 0
	rollback := func() {
		for i := committed - 1; i >= 0; i-- {
			s := staged[i]
			if s.hadOld {
				_ = os.Rename(s.backup, s.dest)
			} else {
				_ = os.Remove(s.dest)
			}
		}
	}

	for i := range staged {
		s := &staged[i]
		backup := s.dest + ".vamos-bak"
		_ = os.Remove(backup)
		if err := os.Rename(s.dest, backup); err == nil {
			s.backup = backup
			s.hadOld = true
		} else if !errors.Is(err, os.ErrNotExist) {
			rollback()
			return fmt.Errorf("backup %s: %w", s.dest, err)
		}
		if err := os.Rename(s.tmp, s.dest); err != nil {
			if s.hadOld {
				_ = os.Rename(s.backup, s.dest)
			}
			rollback()
			return fmt.Errorf("install %s: %w", s.dest, err)
		}
		committed++
	}

	for _, s := range staged {
		if s.hadOld {
			// On Windows the running executable cannot be deleted until it exits;
			// best-effort removal is fine because the next update overwrites .vamos-bak.
			_ = os.Remove(s.backup)
		}
	}
	return nil
}

func stageTempBinary(dir string, content []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(dir, ".vamos-update-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	cleanup = false
	return tmpPath, nil
}
