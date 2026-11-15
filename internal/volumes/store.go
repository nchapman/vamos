// Package volumes retains private volume provenance independently of deployment history.
package volumes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Dir           = "/var/lib/vamos/volumes"
	MaxRecords    = 64
	MaxStoreBytes = 128 << 10
)

type Record struct {
	ID        string `json:"id"`
	Service   string `json:"service"`
	Name      string `json:"name"`
	Mount     string `json:"mount"`
	Operation string `json:"operation"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

func (r Record) DockerName() string { return "vamos-vol-" + r.ID }

type State struct {
	Version int               `json:"version"`
	App     string            `json:"app"`
	HostID  string            `json:"host_id"`
	Records map[string]Record `json:"records"`
}
type Store struct{ Root string }

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var idRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
var ErrInvalidStore = errors.New("invalid volume registry content or identity")
var ErrUnsafeStore = errors.New("volume registry requires private executor-owned directories and regular files without symlinks")

func validName(s string) bool { return len(s) <= 63 && nameRE.MatchString(s) }
func Validate(s *State) error {
	if s == nil || s.Version != 1 || !validName(s.App) || !idRE.MatchString(s.HostID) || s.Records == nil || len(s.Records) > MaxRecords {
		return ErrInvalidStore
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for key, r := range s.Records {
		if key != r.Service || !validName(r.Service) || !validName(r.Name) || !idRE.MatchString(r.ID) || !idRE.MatchString(r.Operation) || names[r.Name] || ids[r.ID] {
			return ErrInvalidStore
		}
		if !utf8.ValidString(r.Mount) || !path.IsAbs(r.Mount) || path.Clean(r.Mount) != r.Mount || r.Mount == "/" || strings.ContainsRune(r.Mount, 0) {
			return ErrInvalidStore
		}
		switch r.State {
		case "planned":
			if r.CreatedAt != "" {
				return ErrInvalidStore
			}
		case "created":
			if _, err := time.Parse(time.RFC3339, r.CreatedAt); err != nil {
				return ErrInvalidStore
			}
		default:
			return ErrInvalidStore
		}
		names[r.Name], ids[r.ID] = true, true
	}
	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxStoreBytes {
		return ErrInvalidStore
	}
	return nil
}
func (s Store) directory() string {
	if s.Root == "" {
		return Dir
	}
	return s.Root
}
func (s Store) Load(app, hostID string) (*State, error) {
	if !validName(app) || !idRE.MatchString(hostID) {
		return nil, ErrInvalidStore
	}
	dir := s.directory()
	if err := ancestors(dir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !private(info, true) || !productionOwner(dir) || !privateParent(dir) {
		return nil, ErrUnsafeStore
	}
	file := filepath.Join(dir, app+".json")
	info, err = os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !private(info, false) {
		return nil, ErrUnsafeStore
	}
	if info.Size() > MaxStoreBytes {
		return nil, ErrInvalidStore
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !private(opened, false) || !os.SameFile(info, opened) {
		return nil, ErrUnsafeStore
	}
	if opened.Size() > MaxStoreBytes {
		return nil, ErrInvalidStore
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxStoreBytes+1))
	if err != nil || len(data) > MaxStoreBytes || !utf8.Valid(data) || !uniqueJSON(data) {
		return nil, ErrInvalidStore
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var state State
	if dec.Decode(&state) != nil {
		return nil, ErrInvalidStore
	}
	if err := Validate(&state); err != nil {
		return nil, err
	}
	if state.App != app || state.HostID != hostID {
		return nil, ErrInvalidStore
	}
	return &state, nil
}
func (s Store) Save(state *State) error {
	if err := Validate(state); err != nil {
		return err
	}
	// Never overwrite unsafe, malformed, or foreign provenance.
	if _, err := s.Load(state.App, state.HostID); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return ErrInvalidStore
	}
	dir := s.directory()
	if err := ancestors(dir); err != nil {
		return err
	}
	if !productionOwner(dir) || !privateParent(dir) {
		return ErrUnsafeStore
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !private(info, true) {
		return ErrUnsafeStore
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	return replace(dir, filepath.Join(dir, state.App+".json"), data)
}
