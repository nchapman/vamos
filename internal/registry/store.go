// Package registry stores local server registrations without contacting servers.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

const (
	registryFile = "registry.json"
	lockFile     = "registry.lock"
	maxSize      = 1 << 20
)

var serverName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Server is a local registration. Target and Domain are validated by the caller
// for SSH and deployment use; the registry only requires nonempty values.
type Server struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Domain string `json:"domain"`
}

// State is the versioned on-disk registry. Default names an entry in Servers,
// or is empty when there are no servers.
type State struct {
	Version int               `json:"version"`
	Default string            `json:"default"`
	Servers map[string]Server `json:"servers"`
}

// Store serializes all access using a persistent advisory lock file. All writers
// must use this protocol; the lock file must never be removed or replaced.
type Store struct {
	dir string
}

// New selects a registry directory without creating or reading it.
func New(dir string) *Store { return &Store{dir: dir} }

// Default selects the vamos directory under the user's configuration directory.
func Default() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find user config directory: %w", err)
	}
	return New(filepath.Join(dir, "vamos")), nil
}

// Read returns a validated snapshot. An absent registry is an empty version 1
// state, not an error. Read also takes the exclusive registry lock.
func (s *Store) Read() (state State, err error) {
	err = s.locked(func() error {
		var readErr error
		state, readErr = s.read()
		return readErr
	})
	return state, err
}

// Put adds a server, making the first registration the default. An identical
// registration is a no-op; neither names nor targets may be reassigned.
func (s *Store) Put(server Server) error {
	return s.locked(func() error {
		state, err := s.read()
		if err != nil {
			return err
		}
		if err := validateServer(server); err != nil {
			return err
		}
		if previous, ok := state.Servers[server.Name]; ok {
			if previous == server {
				return nil
			}
			return fmt.Errorf("server name %q is already registered with a different target or domain", server.Name)
		}
		for name, previous := range state.Servers {
			if previous.Target == server.Target {
				return fmt.Errorf("target %q is already registered as %q", server.Target, name)
			}
		}
		state.Servers[server.Name] = server
		if state.Default == "" {
			state.Default = server.Name
		}
		return s.write(state)
	})
}

// Remove forgets a registration, without any remote operation. Missing names
// are a no-op. Removing the default selects the lexicographically first name.
func (s *Store) Remove(name string) error {
	return s.locked(func() error {
		state, err := s.read()
		if err != nil {
			return err
		}
		if _, ok := state.Servers[name]; !ok {
			return nil
		}
		delete(state.Servers, name)
		if state.Default == name {
			names := make([]string, 0, len(state.Servers))
			for remaining := range state.Servers {
				names = append(names, remaining)
			}
			sort.Strings(names)
			state.Default = ""
			if len(names) > 0 {
				state.Default = names[0]
			}
		}
		return s.write(state)
	})
}

func (s *Store) locked(fn func() error) (err error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspect registry directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("registry directory must be a directory, not a symlink")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.dir, 0700); err != nil {
			return fmt.Errorf("secure registry directory: %w", err)
		}
	}
	path := filepath.Join(s.dir, lockFile)
	if _, err := regularFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect registry lock: %w", err)
	}
	lock := flock.New(path, flock.SetPermissions(0600))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock registry: %w", err)
	}
	defer func() {
		if unlockErr := lock.Close(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock registry: %w", unlockErr))
		}
	}()
	return fn()
}

// regularFile uses Lstat so preexisting symlinks (including dangling ones) and
// special files are rejected before opening them.
func regularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file, not a symlink or special file", path)
	}
	return info, nil
}

func (s *Store) read() (State, error) {
	empty := State{Version: 1, Servers: make(map[string]Server)}
	path := filepath.Join(s.dir, registryFile)
	info, err := regularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("inspect registry: %w", err)
	}
	if info.Size() > maxSize {
		return State{}, errors.New("registry exceeds 1 MiB size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return State{}, fmt.Errorf("open registry: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return State{}, fmt.Errorf("inspect opened registry: %w", err)
	}
	if !os.SameFile(info, opened) {
		return State{}, errors.New("registry changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return State{}, fmt.Errorf("read registry: %w", err)
	}
	if len(data) > maxSize {
		return State{}, errors.New("registry exceeds 1 MiB size limit")
	}
	state, err := decodeState(data)
	if err != nil {
		return State{}, fmt.Errorf("invalid registry: %w", err)
	}
	return state, nil
}

func validateServer(server Server) error {
	if !utf8.ValidString(server.Target) || !utf8.ValidString(server.Domain) {
		return errors.New("server target and domain must be valid UTF-8")
	}
	if !serverName.MatchString(server.Name) {
		return fmt.Errorf("server name %q must be a lowercase DNS label (1-63 characters)", server.Name)
	}
	if strings.TrimSpace(server.Target) == "" || strings.TrimSpace(server.Domain) == "" {
		return errors.New("server target and domain must be nonempty")
	}
	return nil
}

// object rejects duplicate keys as well as unknown, missing, and incorrectly
// cased fields. encoding/json's default struct decoding permits several of these.
func object(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	values := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected object key")
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one JSON object")
	}
	if len(fields) > 0 {
		for _, field := range fields {
			value, ok := values[field]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("missing or null field %q", field)
			}
		}
		if len(values) != len(fields) {
			return nil, errors.New("unknown registry field")
		}
	}
	return values, nil
}

func decodeState(data []byte) (State, error) {
	if !utf8.Valid(data) {
		return State{}, errors.New("registry must be valid UTF-8")
	}
	fields, err := object(data, "version", "default", "servers")
	if err != nil {
		return State{}, err
	}
	state := State{Servers: make(map[string]Server)}
	if err := json.Unmarshal(fields["version"], &state.Version); err != nil {
		return State{}, err
	}
	if state.Version != 1 {
		return State{}, fmt.Errorf("unsupported registry version %d", state.Version)
	}
	if err := json.Unmarshal(fields["default"], &state.Default); err != nil {
		return State{}, err
	}
	servers, err := object(fields["servers"])
	if err != nil {
		return State{}, err
	}
	targets := make(map[string]string)
	for name, raw := range servers {
		if _, err := object(raw, "name", "target", "domain"); err != nil {
			return State{}, fmt.Errorf("server %q: %w", name, err)
		}
		var server Server
		if err := json.Unmarshal(raw, &server); err != nil {
			return State{}, err
		}
		if err := validateServer(server); err != nil {
			return State{}, err
		}
		if name != server.Name {
			return State{}, fmt.Errorf("server key %q does not match name %q", name, server.Name)
		}
		if previous, exists := targets[server.Target]; exists {
			return State{}, fmt.Errorf("servers %q and %q have duplicate targets", previous, name)
		}
		targets[server.Target] = name
		state.Servers[name] = server
	}
	if len(state.Servers) == 0 && state.Default == "" {
		return state, nil
	}
	if _, ok := state.Servers[state.Default]; !ok {
		return State{}, errors.New("default must name a registered server")
	}
	return state, nil
}

func (s *Store) write(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxSize {
		return errors.New("registry exceeds 1 MiB size limit")
	}
	file, err := os.CreateTemp(s.dir, ".registry-*")
	if err != nil {
		return fmt.Errorf("create registry temporary file: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("secure registry temporary file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync registry: %w", err)
	}
	// Close before renaming: Windows does not allow replacing an open file.
	if err := file.Close(); err != nil {
		return fmt.Errorf("close registry: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(s.dir, registryFile)); err != nil {
		return fmt.Errorf("replace registry: %w", err)
	}
	// Windows does not support syncing a directory through os.File.Sync.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open registry directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return fmt.Errorf("sync registry directory: %w", err)
	}
	return nil
}
