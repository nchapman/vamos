// Package secrets retains private, immutable app-scoped secret versions.
package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"unicode/utf8"
)

const (
	MaxValueBytes = 8192
	MaxVersions   = 256
	MaxNames      = 64
	MaxStoreBytes = 1 << 20
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var versionRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
var envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidName(s string) bool    { return len(s) <= 63 && nameRE.MatchString(s) }
func ValidVersion(s string) bool { return versionRE.MatchString(s) }
func ValidateValue(v []byte) error {
	if len(v) == 0 {
		return errors.New("secret value must not be empty")
	}
	if len(v) > MaxValueBytes {
		return errors.New("secret value exceeds 8192 bytes")
	}
	if !utf8.Valid(v) || bytes.ContainsAny(v, "\r\n\x00") {
		return errors.New("secret value must be single-line UTF-8 without CR, LF, or NUL")
	}
	return nil
}
func ReadInput(r io.Reader) ([]byte, error) {
	v, err := io.ReadAll(io.LimitReader(r, MaxValueBytes+3))
	if err != nil {
		return nil, errors.New("cannot read secret input")
	}
	if len(v) > MaxValueBytes+2 {
		return nil, errors.New("secret input exceeds size limit")
	}
	if bytes.HasSuffix(v, []byte("\n")) {
		v = v[:len(v)-1]
		if bytes.HasSuffix(v, []byte("\r")) {
			v = v[:len(v)-1]
		}
	}
	if err := ValidateValue(v); err != nil {
		return nil, err
	}
	return v, nil
}

type Ref struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type Entry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Latest  bool   `json:"latest"`
}
type diskValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// diskState keeps singleton-only stores at v1. Durable batch membership requires
// v2; subsequent singleton writes preserve v2. Both formats remain readable.
type diskState struct {
	Batches    map[string][]string  `json:"batches,omitempty"`
	Version    int                  `json:"version"`
	App        string               `json:"app"`
	RootHostID string               `json:"root_host_id"`
	Latest     map[string]string    `json:"latest"`
	Versions   map[string]diskValue `json:"versions"`
}
type State struct{ data diskState }
type Store struct{ Dir string }

func (s *State) Snapshot(mapping map[string]string) (map[string]Ref, error) {
	out := map[string]Ref{}
	for key, name := range mapping {
		if !envRE.MatchString(key) || !ValidName(name) {
			return nil, errors.New("invalid secret mapping")
		}
		if s == nil || s.data.Latest[name] == "" {
			return nil, errors.New("required secret is not staged")
		}
		out[key] = Ref{name, s.data.Latest[name]}
	}
	return out, nil
}
func (s *State) Values(refs map[string]Ref) (map[string]string, error) {
	out := map[string]string{}
	for key, ref := range refs {
		if !envRE.MatchString(key) || !ValidName(ref.Name) || !ValidVersion(ref.Version) || s == nil {
			return nil, errors.New("required secret version is unavailable")
		}
		v, ok := s.data.Versions[ref.Version]
		if !ok || v.Name != ref.Name {
			return nil, errors.New("required secret version is unavailable")
		}
		out[key] = v.Value
	}
	return out, nil
}
func (s *State) List() []Entry {
	out := []Entry{}
	if s == nil {
		return out
	}
	for id, v := range s.data.Versions {
		out = append(out, Entry{v.Name, id, s.data.Latest[v.Name] == id})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

var invalid = errors.New("invalid secret store content or identity")
var unsafe = errors.New("secret store requires private executor-owned directories and regular files without symlinks")

func (s Store) directory() string {
	if s.Dir == "" {
		return "/var/lib/vamos/secrets"
	}
	return s.Dir
}
func ancestors(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return unsafe
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return unsafe
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func private(info os.FileInfo, dir bool) bool {
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	return info.IsDir() == dir && (dir || info.Mode().IsRegular()) && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && (runtime.GOOS == "windows" || info.Mode().Perm() == mode) && owned(info)
}
func privateParent(dir string) bool {
	info, err := os.Lstat(filepath.Dir(dir))
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && owned(info) && (runtime.GOOS == "windows" || info.Mode().Perm()&0022 == 0)
}

func (s Store) Load(app, hostID string) (*State, error) {
	if !ValidName(app) || !ValidVersion(hostID) {
		return nil, invalid
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
		return nil, unsafe
	}
	path := filepath.Join(dir, app+".json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !private(info, false) {
		return nil, unsafe
	}
	if info.Size() > MaxStoreBytes {
		return nil, invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read secret store")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !private(opened, false) {
		return nil, unsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxStoreBytes+1))
	if err != nil || len(data) > MaxStoreBytes || !utf8.Valid(data) {
		return nil, invalid
	}
	// Reject duplicate object keys as well as unknown fields and trailing data.
	if !uniqueJSON(data) {
		return nil, invalid
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d diskState
	if dec.Decode(&d) != nil {
		return nil, invalid
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, invalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil, invalid
	}
	_, hasBatches := fields["batches"]
	if (d.Version != 1 && d.Version != 2) || (d.Version == 1 && hasBatches) || (d.Version == 2 && len(d.Batches) == 0) || len(d.Batches) > MaxVersions/2 || d.App != app || d.RootHostID != hostID || d.Latest == nil || d.Versions == nil || len(d.Latest) > MaxNames || len(d.Versions) > MaxVersions {
		return nil, invalid
	}
	for id, v := range d.Versions {
		if !ValidVersion(id) || !ValidName(v.Name) || ValidateValue([]byte(v.Value)) != nil || d.Latest[v.Name] == "" {
			return nil, invalid
		}
	}
	for name, id := range d.Latest {
		v, ok := d.Versions[id]
		if !ValidName(name) || !ok || v.Name != name {
			return nil, invalid
		}
	}
	members := make(map[string]bool)
	for op, names := range d.Batches {
		if !ValidVersion(op) || len(names) < 2 || len(names) > MaxNames {
			return nil, invalid
		}
		if _, ok := d.Versions[op]; ok {
			return nil, invalid
		}
		for i, name := range names {
			if !ValidName(name) || (i > 0 && names[i-1] >= name) {
				return nil, invalid
			}
			version := VersionFor(op, name)
			v, ok := d.Versions[version]
			if !ok || v.Name != name || members[version] {
				return nil, invalid
			}
			members[version] = true
		}
	}
	return &State{data: d}, nil
}
func uniqueJSON(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) bool
	walk = func(depth int) bool {
		// The schema is only three objects deep; reject malformed nesting early.
		if depth > 16 {
			return false
		}
		t, e := dec.Token()
		if e != nil {
			return false
		}
		d, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return false
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			for dec.More() {
				if !walk(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, e = dec.Token()
		return e == nil
	}
	if !walk(0) {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}
func (s Store) Stage(app, hostID, name, operation string, value []byte) (Entry, error) {
	entries, err := s.StageBatch(app, hostID, operation, map[string][]byte{name: value})
	if err != nil {
		return Entry{}, err
	}
	return entries[0], nil
}

// StageBatch stages all values with a single atomic store replacement.
func (s Store) StageBatch(app, hostID, operation string, values map[string][]byte) ([]Entry, error) {
	if !ValidName(app) || !ValidVersion(hostID) || !ValidVersion(operation) {
		return nil, invalid
	}
	names, err := batchNames(values)
	if err != nil {
		return nil, err
	}
	state, err := s.Load(app, hostID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &State{data: diskState{Version: 1, App: app, RootHostID: hostID, Latest: map[string]string{}, Versions: map[string]diskValue{}}}
	}
	d := &state.data
	if prior, ok := d.Batches[operation]; ok {
		if len(prior) != len(names) {
			return nil, ErrOperationConflict
		}
		for i := range names {
			if prior[i] != names[i] {
				return nil, ErrOperationConflict
			}
		}
	}
	if len(names) > 1 {
		if _, ok := d.Versions[operation]; ok {
			return nil, ErrOperationConflict
		}
	}
	entries := make([]Entry, 0, len(names))
	existing, newNames := 0, 0
	for _, name := range names {
		version := operation
		if len(names) > 1 {
			version = VersionFor(operation, name)
		}
		if _, ok := d.Batches[version]; ok {
			return nil, ErrOperationConflict
		}
		if old, ok := d.Versions[version]; ok {
			if old.Name != name || old.Value != string(values[name]) {
				return nil, ErrOperationConflict
			}
			existing++
		}
		if _, ok := d.Latest[name]; !ok {
			newNames++
		}
		entries = append(entries, Entry{name, version, d.Latest[name] == version})
	}
	if existing == len(names) {
		return entries, nil
	}
	if existing != 0 {
		return nil, ErrOperationConflict
	}
	if len(d.Versions)+len(names) > MaxVersions {
		return nil, ErrVersionLimit
	}
	if len(d.Latest)+newNames > MaxNames {
		return nil, ErrNameLimit
	}
	if len(names) > 1 {
		if d.Batches == nil {
			d.Batches = make(map[string][]string)
		}
		d.Batches[operation] = names
		d.Version = 2
	}
	for i := range entries {
		e := &entries[i]
		d.Versions[e.Version] = diskValue{e.Name, string(values[e.Name])}
		d.Latest[e.Name] = e.Version
		e.Latest = true
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil, invalid
	}
	if len(data) > MaxStoreBytes {
		return nil, ErrSizeLimit
	}
	dir := s.directory()
	if err := ancestors(dir); err != nil {
		return nil, err
	}
	if !productionOwner(dir) || !privateParent(dir) {
		return nil, unsafe
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.New("cannot create secret directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !private(info, true) {
		return nil, unsafe
	}
	// The first store directory must be durably linked before acknowledging data.
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return nil, errors.New("cannot persist secret directory")
	}
	if err := replace(dir, filepath.Join(dir, app+".json"), data); err != nil {
		return nil, errors.New("cannot persist secret store")
	}
	return entries, nil
}
func replace(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".secret-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

var (
	ErrOperationConflict = errors.New("secret operation identity already has different content")
	ErrVersionLimit      = errors.New("secret store retained-version budget exhausted (256 versions; no automatic deletion)")
	ErrNameLimit         = errors.New("secret store name budget exhausted (64 names)")
	ErrSizeLimit         = errors.New("secret store serialized-size budget exhausted (1 MiB)")
	ErrInvalidStore      = invalid
	ErrUnsafeStore       = unsafe
)
