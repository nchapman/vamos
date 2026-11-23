package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/jobresults"
)

func TestJobInboxScopeAndCombinedLimit(t *testing.T) {
	store := jobresults.Store{Root: filepath.Join(t.TempDir(), "results")}
	j := &journal{Version: 8, HostID: strings.Repeat("a", 32), Status: Status{App: "shop", Phase: "ready"}, ServiceStates: map[string]*serviceState{"web": {Current: &release{}}, "worker": {Current: &release{}}}}
	write := func(n int, host, app, svc, inc string) string {
		t.Helper()
		run := jobresults.Run{ID: fmt.Sprintf("j_%016x", n), Scheduled: time.Unix(int64(n), 0), Output: []byte("private"), Service: svc, Job: "daily", Release: strings.Repeat("b", 32), Outcome: "succeeded"}
		if err := store.Create(host, app, inc, run); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	for n := range 102 {
		svc := []string{"web", "worker"}[n%2]
		write(n, j.HostID, j.App, svc, serviceIncarnation(j, svc))
	}
	inc := serviceIncarnation(j, "web")
	hidden := []string{write(102, strings.Repeat("c", 32), j.App, "web", inc), write(103, j.HostID, "other", "web", inc), write(104, j.HostID, j.App, "web", strings.Repeat("d", 32)), write(105, j.HostID, j.App, "removed", inc)}
	read := func(service, id string) (Response, error) {
		return readJobInbox(store, j, Request{Action: "jobs", Service: service, JobID: id})
	}
	all, e := read("", "")
	if e != nil || len(all.JobRuns) != 100 {
		t.Fatalf("list: %d, %v", len(all.JobRuns), e)
	}
	for n, r := range all.JobRuns {
		if r.Scheduled.Unix() != int64(101-n) || r.Output != nil {
			t.Fatal("incorrect summary", n)
		}
	}
	web, e := read("web", "")
	if e != nil || len(web.JobRuns) != 51 {
		t.Fatal("web list", e)
	}
	for _, r := range web.JobRuns {
		if r.Service != "web" {
			t.Fatal("wrong service")
		}
	}
	id := web.JobRuns[0].ID
	show, e := read("", id)
	if e != nil || show.JobRun == nil || string(show.JobRun.Output) != "private" {
		t.Fatal("show", e)
	}
	if _, e = read("worker", id); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("cross-service show", e)
	}
	for _, id := range hidden {
		if _, e = read("", id); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("foreign show", id, e)
		}
	}
	for _, state := range []struct {
		v     int
		phase string
	}{{1, "ready"}, {8, "destroyed"}} {
		j.Version, j.Phase = state.v, state.phase
		if _, e = read("", id); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("legacy/tombstone show", e)
		}
	}
}
