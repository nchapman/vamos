package jobresults

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const host = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const inc = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func run(n int) Run {
	return Run{ID: fmt.Sprintf("j_%016x", n), Job: "backup", Service: "svc", Release: host, Scheduled: time.Unix(int64(n), 0)}
}
func TestWriterProcess(t *testing.T) {
	root := os.Getenv("JOBRESULTS_TEST_ROOT")
	if root == "" {
		return
	}
	s := Store{root}
	start := 0
	if os.Getenv("JOBRESULTS_SECOND") == "1" {
		start = 51
	}
	for i := start; i < start+51; i++ {
		if err := s.Create(host, "app", inc, run(i)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestConcurrentRetention(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "results")}
	var cmds []*exec.Cmd
	for i := 0; i < 2; i++ {
		c := exec.Command(os.Args[0], "-test.run=^TestWriterProcess$")
		c.Env = append(os.Environ(), "JOBRESULTS_TEST_ROOT="+s.Root, fmt.Sprintf("JOBRESULTS_SECOND=%d", i))
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	rs, e := s.List(host, "app", "svc", inc)
	if e != nil || len(rs) != MaxRuns {
		t.Fatalf("%d: %v", len(rs), e)
	}
	for i, r := range rs {
		if r.ID != run(101-i).ID {
			t.Fatal(r)
		}
	}
	if e = s.Finish(host, "app", inc, run(0)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func TestPayloadAndScope(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "results")}
	r := run(1)
	if e := s.Create(host, "app", inc, r); e != nil {
		t.Fatal(e)
	}
	r.Output = append(bytes.Repeat([]byte("a"), MaxOutput), 'z')
	if e := s.Finish(host, "app", inc, r); e != nil {
		t.Fatal(e)
	}
	g, e := s.Get(host, "app", "svc", inc, r.ID)
	if e != nil || !g.Truncated || !bytes.Equal(g.Output, r.Output[1:]) {
		t.Fatalf("payload %d: %v", len(g.Output), e)
	}
	d := filepath.Join(s.Root, host, "app", "svc", inc)
	files, e := os.ReadDir(d)
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range files {
		if f.Name() != "index" {
			if e = os.Remove(filepath.Join(d, f.Name())); e != nil {
				t.Fatal(e)
			}
		}
	}
	rs, e := s.List(host, "app", "svc", inc)
	if e != nil || len(rs) != 1 || rs[0].Output != nil {
		t.Fatal(rs, e)
	}
	if _, e = s.Get(host, "app", "svc", host, r.ID); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if e = s.Remove(host, "app", "svc", inc); e != nil {
		t.Fatal(e)
	}
	if e = s.Finish(host, "app", inc, r); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if _, e = os.Stat(d); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func TestPurgePreservesUnrecognizedFiles(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "results")}
	if e := s.Create(host, "app", inc, run(0)); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(s.Root, host, "app", "svc", inc, "foreign")
	if e := os.WriteFile(file, []byte("unrelated"), 0600); e != nil {
		t.Fatal(e)
	}
	if err := s.Remove(host, "app", "svc", inc); err == nil {
		t.Fatal("purged unrecognized files")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "results")}
	for _, k := range [][4]string{{"../", "app", "svc", inc}, {host, "App", "svc", inc}, {host, "app", "../svc", inc}, {host, "app", "svc", ""}, {host, strings.Repeat("a", 64), "svc", inc}} {
		if _, e := s.List(k[0], k[1], k[2], k[3]); e == nil {
			t.Fatal(k)
		}
	}
	if e := os.Mkdir(s.Root, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(t.TempDir(), filepath.Join(s.Root, host)); e != nil {
		t.Fatal(e)
	}
	if e := s.Create(host, "app", inc, run(0)); e == nil {
		t.Fatal("followed symlink")
	}
	id, e := NewID()
	if e != nil || !ValidID(id) || ValidID(id+"/") {
		t.Fatal(id, e)
	}
}
