package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/deploy"
)

type serviceBinding struct {
	HostIdentity string `json:"host_identity"`
	Incarnation  string `json:"incarnation"`
}
type serviceBindings map[string]serviceBinding

// Target and app are hashed, never interpolated into filesystem paths.
func serviceBindingPath(dir, target, app string) string {
	return filepath.Join(dir, ".soar", fmt.Sprintf("services-%x.json", sha256.Sum256([]byte(target+"\x00"+app))))
}
func safeMetadata(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe project metadata: %s", path)
	}
	return nil
}
func readServiceBindings(dir, target, app string) (serviceBindings, error) {
	result := serviceBindings{}
	root := filepath.Join(dir, ".soar")
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() {
			return nil, errors.New(".soar must not be a symlink")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	path := serviceBindingPath(dir, target, app)
	if err := safeMetadata(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("invalid service bindings")
	}
	for _, b := range result {
		if b.HostIdentity == "" || b.Incarnation == "" {
			return nil, errors.New("invalid service identity binding")
		}
	}
	return result, nil
}
func writeServiceBindings(dir, target, app string, updates serviceBindings) error {
	root := filepath.Join(dir, ".soar")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return errors.New(".soar must not be a symlink")
	}
	lockPath := filepath.Join(root, "services.lock")
	if err := safeMetadata(lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Close()
	current, err := readServiceBindings(dir, target, app)
	if err != nil {
		return err
	}
	ignore := filepath.Join(root, ".gitignore")
	if err := safeMetadata(ignore); err != nil {
		return err
	}
	if data, err := os.ReadFile(ignore); err == nil {
		if string(data) != "*\n" && string(data) != "*" {
			return errors.New(".soar/.gitignore must contain '*'")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("*\n"), 0600); err != nil {
			return err
		}
	} else {
		return err
	}
	for name, b := range updates {
		current[name] = b
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".services-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
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
	if err := os.Rename(f.Name(), serviceBindingPath(dir, target, app)); err != nil {
		return err
	}
	// Directory fsync is unavailable on Windows, matching registry metadata.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(root)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func acknowledgedBindings(r deploy.Request, s *deploy.Status, selected []string) serviceBindings {
	result := serviceBindings{}
	if s == nil || s.HostIdentity == "" {
		return result
	}
	for _, name := range selected {
		id := s.Services[name].Incarnation
		if id != "" && ((r.Operation != "" && s.Operation == r.Operation) || (r.HostIdentity == s.HostIdentity && r.Bindings[name] == id)) {
			result[name] = serviceBinding{s.HostIdentity, id}
		}
	}
	return result
}
