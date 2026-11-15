package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/registry"
)

type binding struct {
	Version int    `json:"version"`
	App     string `json:"app"`
	Server  string `json:"server"`
	Target  string `json:"target"`
}

func readProjectMetadata(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, errors.New("project metadata exceeds 4 KiB")
	}
	return data, nil
}

func readBinding(dir string) (*binding, error) {
	root := filepath.Join(dir, ".vamos")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("project .vamos must be a directory, not a symlink")
	}
	path := filepath.Join(root, "server.json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("invalid project server binding file")
	}
	data, err := readProjectMetadata(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value binding
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("read project binding: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing project binding data")
	}
	if value.Version != 1 || value.App == "" || value.Server == "" || value.Target == "" {
		return nil, errors.New("invalid project server binding")
	}
	return &value, nil
}

func bindProject(dir, app string, server registry.Server) error {
	root := filepath.Join(dir, ".vamos")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("project .vamos must not be a symlink")
	}
	lockPath := filepath.Join(root, "binding.lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return errors.New("project binding lock is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Close()
	current, err := readBinding(dir)
	if err != nil {
		return err
	}
	wanted := binding{Version: 1, App: app, Server: server.Name, Target: server.Target}
	if current != nil {
		if *current != wanted {
			return errors.New("project is bound to another app/server; --server overrides only this invocation")
		}
		return nil
	}
	// Keep local target metadata out of Git even in projects whose root ignore
	// file knows nothing about Vamos. Never overwrite an existing ignore file.
	ignore := filepath.Join(root, ".gitignore")
	f, err := os.OpenFile(ignore, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.WriteString("*\n")
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if errors.Is(err, os.ErrExist) {
		info, err := os.Lstat(ignore)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("unsafe .vamos/.gitignore")
		}
		data, err := readProjectMetadata(ignore)
		if err != nil {
			return err
		}
		if string(bytes.TrimSpace(data)) != "*" {
			return errors.New("existing .vamos/.gitignore must contain '*' to protect project metadata")
		}
	} else {
		return err
	}
	data, err := json.MarshalIndent(wanted, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".binding-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(root, "server.json")); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(root)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}

func selectAppServer(state registry.State, dir, app, override string) (registry.Server, error) {
	name := override
	if name == "" && dir != "" {
		b, err := readBinding(dir)
		if err != nil {
			return registry.Server{}, err
		}
		if b != nil {
			if b.App != app {
				return registry.Server{}, errors.New("project app name differs from its binding; refusing an implicit app rename")
			}
			server, ok := state.Servers[b.Server]
			if !ok {
				return registry.Server{}, errors.New("bound server is not registered; restore it or explicitly select --server")
			}
			if server.Target != b.Target {
				return registry.Server{}, errors.New("bound server target changed; use --server explicitly after reviewing it")
			}
			return server, nil
		}
	}
	if name == "" {
		name = state.Default
	}
	server, ok := state.Servers[name]
	if !ok {
		return registry.Server{}, errors.New("no selected registered server; run vamos server add first or select --server")
	}
	return server, nil
}
