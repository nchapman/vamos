package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestVerboseBuildDiagnosticBoundary(t *testing.T) {
	m, r := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		log := []byte(strings.Repeat("build step output\n", 5000) + "useful ending")
		return process.Result{Stderr: log[len(log)-process.OutputLimit:]}, errors.New("failed")
	}
	resp, _ := m.Execute(context.Background(), r, []byte("archive"))
	if len(resp.Error) > 4096 {
		t.Errorf("unbounded response: %d", len(resp.Error))
	}
	r.Action = "status"
	r.Operation = ""
	r.Config = nil
	resp, e := m.Execute(context.Background(), r, nil)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(resp)
	if len(b) >= 64<<10 || len(resp.Status.LastError) > 4096 || !strings.HasSuffix(resp.Status.LastError, "useful ending") {
		t.Fatalf("unbounded or lost diagnostic: encoded=%d diagnostic=%d", len(b), len(resp.Status.LastError))
	}
}

func TestJournalRejectsUnknownFields(t *testing.T) {
	m, r := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, errors.New("failed")
	}
	m.Execute(context.Background(), r, []byte("archive"))
	j, e := m.load(r.App)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(j)
	b = append(b[:len(b)-1], []byte(`,"unexpected":true}`)...)
	atomic(m.dir(r.App)+"/state.json", b, 0600)
	if _, e = m.load(r.App); e == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestMutationReportsObservedRunning(t *testing.T) {
	f, r := flowSetup(t)
	resp, e := f.m.Execute(context.Background(), r, []byte("archive"))
	if e != nil || resp.Status == nil || !resp.Status.Running {
		t.Fatalf("response=%+v error=%v", resp, e)
	}
	f.containers[resp.Status.Container].State.Running = false
	resp, e = f.m.Execute(context.Background(), flowResume(r), nil)
	if e != nil || resp.Status.Running {
		t.Fatalf("stopped current reported running: %+v %v", resp, e)
	}
}

func TestJournalBoundaryInvariants(t *testing.T) {
	mutations := map[string]func(*journal){
		"version":           func(j *journal) { j.Version = 2 },
		"phase":             func(j *journal) { j.Phase = "unknown" },
		"config":            func(j *journal) { j.Config.Name = "other" },
		"host":              func(j *journal) { j.HostID = "foreign" },
		"archive":           func(j *journal) { j.Archive = "../context.tar" },
		"url":               func(j *journal) { j.URL = "https://notes.example.com\nimport evil" },
		"container":         func(j *journal) { j.Current.Container = "foreign" },
		"image":             func(j *journal) { j.Current.Image = "latest" },
		"port":              func(j *journal) { j.Current.Port = "80\n}" },
		"release operation": func(j *journal) { j.Current.Operation = "bad" },
		"current":           func(j *journal) { j.Current = nil },
		"candidate":         func(j *journal) { j.Phase = "starting" },
		"promotion":         func(j *journal) { j.OldCaddy = "only one" },
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			f, r := flowSetup(t)
			j := f.execute(r, "archive", false)
			mut(j)
			b, _ := json.Marshal(j)
			atomic(f.m.dir(r.App)+"/state.json", b, 0600)
			if _, e := f.m.load(r.App); e == nil {
				t.Fatal("accepted corrupt journal")
			}
		})
	}
	for _, suffix := range []string{" {}", strings.Repeat(" ", 2<<20)} {
		t.Run("decode bound", func(t *testing.T) {
			f, r := flowSetup(t)
			j := f.execute(r, "archive", false)
			b, _ := json.Marshal(j)
			atomic(f.m.dir(r.App)+"/state.json", append(b, []byte(suffix)...), 0600)
			if _, e := f.m.load(r.App); e == nil {
				t.Fatal("accepted invalid encoding")
			}
		})
	}
}
