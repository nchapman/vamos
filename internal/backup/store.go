package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/nchapman/vamos/internal/backupbackend"
)

const maxRecordBytes = 128 * 1024

var errStore = errors.New("repository state is unavailable, unsafe, or belongs to another host")

type record struct {
	Version       int                       `json:"version"`
	HostID        string                    `json:"host_id"`
	Metadata      Metadata                  `json:"metadata"`
	Credentials   backupbackend.Credentials `json:"credentials"`
	InitAttempted bool                      `json:"init_attempted,omitempty"`
}

type store struct{ dir string }

func safeAncestors(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errStore
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errStore
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func private(info os.FileInfo, directory bool) bool {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return info.IsDir() == directory && (directory || info.Mode().IsRegular()) && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && owned(info) && (runtime.GOOS == "windows" || info.Mode().Perm() == mode)
}

func (s store) checkDir(create bool) error {
	if err := safeAncestors(s.dir); err != nil {
		return err
	}
	// The application-independent backup boundary and repository directory are
	// private, even though their existing /var/lib/vamos parent need not be.
	parent := filepath.Dir(s.dir)
	outer, err := os.Lstat(filepath.Dir(parent))
	if err != nil || !outer.IsDir() || !owned(outer) || (runtime.GOOS != "windows" && outer.Mode().Perm()&0022 != 0) {
		return errStore
	}
	for _, dir := range []string{parent, s.dir} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) && create {
			outer, e := os.Lstat(filepath.Dir(dir))
			if e != nil || !outer.IsDir() || !owned(outer) || (runtime.GOOS != "windows" && outer.Mode().Perm()&0022 != 0) {
				return errStore
			}
			if e = os.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				return errStore
			}
			// New ancestor entries must survive a crash before remote init too.
			if e = syncDirectory(filepath.Dir(dir)); e != nil {
				return e
			}
			info, err = os.Lstat(dir)
		}
		if errors.Is(err, os.ErrNotExist) && !create {
			return os.ErrNotExist
		}
		if err != nil || !private(info, true) {
			return errStore
		}
	}
	return nil
}

func validRecord(r *record, name, hostID string) bool {
	if r == nil || r.Version != 1 || !ValidOperation(hostID) || r.HostID != hostID || !ValidName(name) || r.Metadata.Name != name || !ValidOperation(r.Metadata.Operation) {
		return false
	}
	m := r.Metadata
	if backupbackend.ValidateConfig(m.Config) != nil || backupbackend.ValidateCredentials(r.Credentials) != nil {
		return false
	}
	if m.Mode != "create" && m.Mode != "connect" {
		return false
	}
	if m.Mode == "connect" && r.InitAttempted {
		return false
	}
	switch m.State {
	case "prepared":
		if r.InitAttempted {
			return false
		}
	case "initializing":
		if m.Mode != "create" || !r.InitAttempted {
			return false
		}
	case "ready", "unverified":
		if m.RepositoryID == "" || m.Format != 2 || (m.Mode == "create" && !r.InitAttempted) {
			return false
		}
	default:
		return false
	}
	if m.RepositoryID != "" && backupbackend.ValidateIdentity(backupbackend.Identity{ID: m.RepositoryID, Version: m.Format}) != nil {
		return false
	}
	if m.RepositoryID == "" && m.Format != 0 {
		return false
	}
	if m.Mode == "connect" && m.RepositoryID == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err != nil {
		return false
	}
	if m.VerifiedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, m.VerifiedAt); err != nil {
			return false
		}
	}
	if m.State == "ready" && m.VerifiedAt == "" {
		return false
	}
	if m.Mode == "create" {
		request := Request{Action: "create", Name: name, Operation: m.Operation, Config: &m.Config, Confirm: true}
		if ValidateRequest(request) != nil || !repositoryPasswordPattern.MatchString(r.Credentials.Password) {
			return false
		}
	}
	return true
}

func (s store) load(name, hostID string) (*record, error) {
	if !ValidName(name) || !ValidOperation(hostID) {
		return nil, errStore
	}
	if err := s.checkDir(false); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, name+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil || !private(info, false) || info.Size() > maxRecordBytes {
		return nil, errStore
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errStore
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !private(opened, false) {
		return nil, errStore
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil || len(data) > maxRecordBytes {
		return nil, errStore
	}
	var r record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r); err != nil {
		return nil, errStore
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) || !validRecord(&r, name, hostID) {
		return nil, errStore
	}
	return &r, nil
}

func (s store) list(hostID string) ([]record, error) {
	records := []record{}
	if !ValidOperation(hostID) {
		return nil, errStore
	}
	if err := s.checkDir(false); errors.Is(err, os.ErrNotExist) {
		return records, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, errStore
	}
	for _, entry := range entries {
		// An interrupted atomic write is never authoritative. Its private file is
		// retained for operator reconciliation rather than adopted or auto-pruned.
		if len(entry.Name()) > 5 && entry.Name()[:5] == ".tmp-" {
			info, e := entry.Info()
			if e != nil || !private(info, false) {
				return nil, errStore
			}
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			return nil, errStore
		}
		name = name[:len(name)-5]
		r, e := s.load(name, hostID)
		if e != nil {
			return nil, e
		}
		records = append(records, *r)
		if len(records) > MaxRepositories {
			return nil, errStore
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Metadata.Name < records[j].Metadata.Name })
	return records, nil
}

// save requires the caller's host mutation lock; existing records are always
// validated before replacement. Directory sync makes initialization intent durable.
func (s store) save(r *record) error {
	if !validRecord(r, r.Metadata.Name, r.HostID) {
		return errStore
	}
	if err := s.checkDir(true); err != nil {
		return err
	}
	old, err := s.load(r.Metadata.Name, r.HostID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old != nil && (old.Metadata.Operation != r.Metadata.Operation || old.Metadata.Config != r.Metadata.Config || old.Credentials != r.Credentials || old.Metadata.Mode != r.Metadata.Mode || (old.InitAttempted && !r.InitAttempted) || (old.Metadata.RepositoryID != "" && old.Metadata.RepositoryID != r.Metadata.RepositoryID)) {
		return errStore
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(data)+1 > maxRecordBytes {
		return errStore
	}
	f, err := os.CreateTemp(s.dir, ".tmp-")
	if err != nil {
		return errStore
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errStore
	}
	if err = os.Rename(path, filepath.Join(s.dir, r.Metadata.Name+".json")); err != nil {
		return errStore
	}
	return syncDirectory(s.dir)
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return errStore
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil || closeErr != nil {
		return errStore
	}
	return nil
}
