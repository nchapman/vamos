// Package jobresults stores bounded per-service job histories under a private root.
package jobresults

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/gofrs/flock"
)

const MaxOutput = 32 << 10
const MaxRuns = 100
const maxIndex = 1 << 20

type Run struct {
	ID        string     `json:"id"`
	Service   string     `json:"service"`
	Job       string     `json:"job"`
	Release   string     `json:"release"`
	Outcome   string     `json:"outcome"`
	Reason    string     `json:"reason,omitempty"`
	Scheduled time.Time  `json:"scheduled"`
	Started   *time.Time `json:"started,omitempty"`
	Finished  *time.Time `json:"finished,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Output    []byte     `json:"output,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

type Store struct{ Root string }

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var scope = regexp.MustCompile(`^[a-f0-9]{32}$`)
var idPattern = regexp.MustCompile(`^j_[a-f0-9]{16}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func NewID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "j_" + hex.EncodeToString(b[:]), nil
}
func ValidID(id string) bool { return idPattern.MatchString(id) }

type entry struct {
	Run
	Blob string `json:"blob,omitempty"`
}

func syncDir(p string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// Root's parent is trusted. Reject symlinks at Root and at each store-owned component.
func directory(p string, create bool) error {
	st, e := os.Lstat(p)
	if errors.Is(e, os.ErrNotExist) && create {
		if e = os.Mkdir(p, 0700); e == nil {
			return syncDir(filepath.Dir(p))
		}
		if !errors.Is(e, os.ErrExist) {
			return e
		}
		st, e = os.Lstat(p)
	}
	if e != nil {
		return e
	}
	if !st.IsDir() || !privateEntry(st, 0700) {
		return fmt.Errorf("unsafe job results directory: %s", p)
	}
	return nil
}
func regular(p string) error {
	st, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || !privateEntry(st, 0600) {
		return fmt.Errorf("unsafe job results file: %s", p)
	}
	return nil
}
func (s Store) with(h, a, v, i string, create bool, fn func(string) error) error {
	if s.Root == "" || !scope.MatchString(h) || !scope.MatchString(i) || !label.MatchString(a) || !label.MatchString(v) {
		return errors.New("invalid job results scope")
	}
	root := filepath.Clean(s.Root)
	if create {
		if e := os.MkdirAll(filepath.Dir(root), 0700); e != nil {
			return e
		}
	}
	if e := directory(root, create); e != nil {
		return e
	}
	p := filepath.Join(root, ".lock")
	if e := regular(p); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	l := flock.New(p, flock.SetPermissions(0600))
	if e := l.Lock(); e != nil {
		return e
	}
	defer l.Unlock()
	d := root
	for _, k := range []string{h, a, v, i} {
		d = filepath.Join(d, k)
		if e := directory(d, create); e != nil {
			return e
		}
	}
	return fn(d)
}
func read(p string, n int) ([]byte, error) {
	if e := regular(p); e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, int64(n+1)))
	if len(b) > n {
		return nil, errors.New("oversized job results file")
	}
	return b, e
}
func load(d string) ([]entry, error) {
	b, e := read(filepath.Join(d, "index"), maxIndex)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var x []entry
	if e = json.Unmarshal(b, &x); e != nil {
		return nil, e
	}
	if len(x) > MaxRuns {
		return nil, errors.New("oversized inbox")
	}
	for _, v := range x {
		if !ValidID(v.ID) || v.Output != nil || (v.Blob != "" && !digest.MatchString(v.Blob)) {
			return nil, errors.New("invalid inbox entry")
		}
	}
	return x, nil
}
func atomic(d, name string, b []byte) error {
	f, e := os.CreateTemp(d, ".pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), filepath.Join(d, name)); e != nil {
		return e
	}
	return syncDir(d)
}
func sweep(d string, x []entry) error {
	keep := map[string]bool{}
	for _, v := range x {
		keep[v.Blob] = true
	}
	fs, e := os.ReadDir(d)
	if e != nil {
		return e
	}
	for _, f := range fs {
		n := f.Name()
		if (digest.MatchString(n) && !keep[n]) || strings.HasPrefix(n, ".pending-") {
			if e = os.Remove(filepath.Join(d, n)); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s Store) Create(h, a, i string, r Run) error { return s.put(h, a, i, r, true) }
func (s Store) Finish(h, a, i string, r Run) error { return s.put(h, a, i, r, false) }
func (s Store) put(h, a, i string, r Run, create bool) error {
	if !ValidID(r.ID) || !scope.MatchString(r.Release) || !label.MatchString(r.Job) {
		return errors.New("invalid run identity")
	}
	return s.with(h, a, r.Service, i, create, func(d string) error {
		x, e := load(d)
		if e != nil {
			return e
		}
		pos := -1
		for j, v := range x {
			if v.ID == r.ID {
				pos = j
				break
			}
		}
		if create && pos >= 0 {
			return os.ErrExist
		}
		if !create {
			if pos < 0 {
				return os.ErrNotExist
			}
			prev := x[pos].Run
			if prev.Job != r.Job || prev.Release != r.Release || !prev.Scheduled.Equal(r.Scheduled) {
				return errors.New("job attempt identity changed")
			}
		}
		if e = sweep(d, x); e != nil {
			return e
		}
		out := r.Output
		if len(out) > MaxOutput {
			out = out[len(out)-MaxOutput:]
			r.Truncated = true
		}
		r.Output = nil
		v := entry{Run: r}
		if len(out) > 0 {
			v.Blob = fmt.Sprintf("%x", sha256.Sum256(out))
		}
		if pos < 0 {
			x = append(x, v)
		} else {
			x[pos] = v
		}
		sort.SliceStable(x, func(i, j int) bool {
			if x[i].Scheduled.Equal(x[j].Scheduled) {
				return x[i].ID > x[j].ID
			}
			return x[i].Scheduled.After(x[j].Scheduled)
		})
		if len(x) > MaxRuns {
			x = x[:MaxRuns]
		}
		b, e := json.Marshal(x)
		if e != nil {
			return e
		}
		if len(b) > maxIndex {
			return errors.New("job metadata too large")
		}
		if v.Blob != "" {
			if e = atomic(d, v.Blob, out); e != nil {
				return e
			}
		}
		if e = atomic(d, "index", b); e != nil {
			return e
		}
		return sweep(d, x)
	})
}
func (s Store) List(h, a, v, i string) ([]Run, error) {
	out := []Run{}
	e := s.with(h, a, v, i, false, func(d string) error {
		x, e := load(d)
		for _, r := range x {
			out = append(out, r.Run)
		}
		return e
	})
	if errors.Is(e, os.ErrNotExist) {
		e = nil
	}
	return out, e
}
func (s Store) Get(h, a, v, i, id string) (Run, error) {
	var out Run
	if !ValidID(id) {
		return out, errors.New("invalid run ID")
	}
	e := s.with(h, a, v, i, false, func(d string) error {
		x, e := load(d)
		if e != nil {
			return e
		}
		for _, r := range x {
			if r.ID == id {
				out = r.Run
				if r.Blob != "" {
					out.Output, e = read(filepath.Join(d, r.Blob), MaxOutput)
				}
				return e
			}
		}
		return os.ErrNotExist
	})
	return out, e
}
func (s Store) Remove(h, a, v, i string) error {
	e := s.with(h, a, v, i, false, func(d string) error {
		files, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		for _, file := range files {
			name := file.Name()
			if name != "index" && !digest.MatchString(name) && !strings.HasPrefix(name, ".pending-") {
				return errors.New("unexpected file prevents job history purge")
			}
			if err := regular(filepath.Join(d, name)); err != nil {
				return err
			}
		}
		for _, file := range files {
			if err := os.Remove(filepath.Join(d, file.Name())); err != nil {
				return err
			}
		}
		// The root lock also serializes callers on sibling scopes. Prune only
		// empty scope directories; never recursively delete foreign content.
		for d != filepath.Clean(s.Root) {
			entries, err := os.ReadDir(d)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				break
			}
			if err = os.Remove(d); err != nil {
				return err
			}
			d = filepath.Dir(d)
			if err = syncDir(d); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	return e
}
