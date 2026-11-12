// Package lifecycle manages a dedicated Ubuntu host installation. It does not
// adopt existing runtimes or delete application workloads.
package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	StatePath    = "/var/lib/soar/host.json"
	ExecutorPath = "/usr/local/lib/soar/executor"
	LockPath     = "/run/lock/soar-host.lock"
)

type Config struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// State is both the ownership manifest and the resumable operation journal.
// Arbitrary paths are never loaded from it: all managed paths are fixed in code.
type State struct {
	Version             int               `json:"version"`
	ID                  string            `json:"id"`
	Config              Config            `json:"config"`
	Phase               string            `json:"phase"`
	Step                string            `json:"step"`
	Packages            []Package         `json:"packages"`
	Configured          bool              `json:"configured"`
	ExecutorSHA         string            `json:"executor_sha256,omitempty"`
	PurgeApproved       bool              `json:"purge_approved"`
	Accounts            map[string]string `json:"accounts,omitempty"`
	ContainerdConfigSHA string            `json:"containerd_config_sha256,omitempty"`
	LastError           string            `json:"last_error,omitempty"`
}

var (
	labelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(:[a-z0-9]+)?$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9.+:~_-]+$`)
)

func ValidateConfig(c Config) error {
	if len(c.Name) == 0 || len(c.Name) > 63 || !labelRE.MatchString(c.Name) {
		return errors.New("server name must be a lowercase DNS label")
	}
	if len(c.Domain) > 253 || !strings.Contains(c.Domain, ".") {
		return errors.New("base domain must be a DNS hostname without a scheme, port, or wildcard")
	}
	for _, label := range strings.Split(c.Domain, ".") {
		if len(label) == 0 || len(label) > 63 || !labelRE.MatchString(label) {
			return errors.New("base domain must contain lowercase DNS labels")
		}
	}
	return nil
}

func newID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}

func ReadState(filename string) (*State, error) {
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("host state must be a regular file no larger than 1 MiB")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1024*1024+1))
	decoder.DisallowUnknownFields()
	var s State
	if err := decoder.Decode(&s); err != nil {
		return nil, fmt.Errorf("read host state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing host state data")
	}
	if s.Version != 1 || len(s.ID) != 32 {
		return nil, errors.New("invalid host state version or identity")
	}
	if _, err := hex.DecodeString(s.ID); err != nil {
		return nil, errors.New("invalid installation identity")
	}
	if err := ValidateConfig(s.Config); err != nil {
		return nil, err
	}
	switch s.Phase {
	case "installing", "ready", "removing", "removed":
	default:
		return nil, errors.New("invalid installation phase")
	}
	if (s.Phase == "removing" || s.Phase == "removed") != s.PurgeApproved {
		return nil, errors.New("inconsistent purge authorization in host state")
	}
	if (s.Phase == "ready" || s.Phase == "removed") && s.Step != "complete" {
		return nil, errors.New("incomplete terminal host state")
	}
	if s.Phase == "ready" && !s.Configured {
		return nil, errors.New("ready host is not configured")
	}
	if !contains([]string{"planned", "install-packages", "configure", "start-services", "complete", "stop-services", "purge-packages", "remove-owned-data"}, s.Step) {
		return nil, errors.New("invalid host lifecycle step")
	}
	for key := range s.Accounts {
		if key != "passwd:caddy" && key != "group:caddy" && key != "group:docker" {
			return nil, errors.New("invalid service account record")
		}
	}
	if s.Configured {
		if len(s.Accounts) != 3 || len(s.ExecutorSHA) != 64 {
			return nil, errors.New("missing configured resource ownership")
		}
		if _, err := hex.DecodeString(s.ExecutorSHA); err != nil {
			return nil, errors.New("invalid executor digest")
		}
	}
	if s.ContainerdConfigSHA != "" {
		if digest, err := hex.DecodeString(s.ContainerdConfigSHA); err != nil || len(digest) != 32 {
			return nil, errors.New("invalid containerd config digest")
		}
	}
	seen := map[string]bool{}
	for _, p := range s.Packages {
		if !packageRE.MatchString(p.Name) || !versionRE.MatchString(p.Version) || seen[p.Name] {
			return nil, errors.New("invalid package ownership record")
		}
		seen[p.Name] = true
	}
	if len(s.Packages) == 0 {
		return nil, errors.New("host state has no package ownership record")
	}
	return &s, nil
}

func atomicWrite(filename string, data []byte, mode os.FileMode) error {
	if err := rejectSymlink(filename); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".soar-write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filename); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(filename))
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := dir.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// Check every existing ancestor before destructive operations or managed writes.
func rejectSymlink(filename string) error {
	for p := filepath.Clean(filename); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s", p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
