package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/jobresults"
)

func TestJobsTargetAndJSON(t *testing.T) {
	path, _, deps := appFixture(t)
	const id = "j_0123456789abcdef"
	run := jobresults.Run{ID: id, Output: []byte("  hello\n\"world\"\r\n")}
	for _, tc := range []struct {
		args    []string
		app, id string
	}{
		{[]string{"-f", path, "--service", "web", "--json"}, "hello", ""},
		{[]string{"show", id, "--server", "home", "--app", "outside", "--service", "web", "--json"}, "outside", id},
	} {
		deps.Agent = func(_ context.Context, target string, r deploy.Request, _ []byte) (deploy.Response, error) {
			if target != "root@home" || r.Action != "jobs" || r.JobID != tc.id || r.Service != "web" || r.App != tc.app {
				t.Fatalf("unexpected target/request: %s %+v", target, r)
			}
			return deploy.Response{JobRuns: []jobresults.Run{run}, JobRun: &run}, nil
		}
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "jobs", tc.args, &out, &diag, deps); code != 0 {
			t.Fatalf("%d: %s", code, &diag)
		}
		if tc.id == "" {
			var rows []map[string]any
			if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["id"] != id || rows[0]["output"] != nil {
				t.Fatal(out.String(), err)
			}
		} else {
			var row map[string]any
			if err := json.Unmarshal(out.Bytes(), &row); err != nil || row["output"] != string(run.Output) {
				t.Fatal(out.String(), err)
			}
		}
	}
}

func TestJobRenderingSeparatesMetadataAndLogs(t *testing.T) {
	r := jobresults.Run{ID: "j_0123456789abcdef", Job: "timer", Outcome: "skipped", Reason: "paused", Output: []byte("one\n  two\x1b]52;c;data\x07\r\u202e\xff"), Truncated: true}
	response := deploy.Response{JobRuns: []jobresults.Run{r}, JobRun: &r}
	for _, p := range []*presentation{nil, {width: 80}} {
		var out bytes.Buffer
		if err := writeJobs(&out, p, appReport{}, response, r.ID, false); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		if !utf8.ValidString(s) || strings.ContainsAny(s, "\x1b\x07\r\u202e") {
			t.Fatalf("unsafe output: %q", s)
		}
		for _, want := range []string{"one\n  two", "[earlier output truncated]", "paused", "skipped", "Scheduled"} {
			if !strings.Contains(s, want) {
				t.Fatalf("missing %q: %q", want, s)
			}
		}
		out.Reset()
		if err := writeJobs(&out, p, appReport{}, response, "", false); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "two") || !strings.Contains(out.String(), r.ID) {
			t.Fatal(out.String())
		}
		status := jobStatusFields(p, deploy.JobStatus{Last: &r, Paused: "maintenance"})
		for _, want := range []string{"Last run", r.ID, "maintenance", "skipped"} {
			if !strings.Contains(status, want) {
				t.Fatal(status)
			}
		}
		if strings.Contains(status, "two") {
			t.Fatal(status)
		}
	}
}

func TestStatusShowsScheduleAndLastResultWithoutLogs(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
		return deploy.Response{Status: &deploy.Status{Jobs: []deploy.JobStatus{{Name: "timer", Next: "tomorrow", Paused: "maintenance", Last: &jobresults.Run{ID: "j_0123456789abcdef", Outcome: "failed", Output: []byte("sensitive")}}}}}, nil
	}
	for _, jsonMode := range []bool{false, true} {
		args := []string{"-f", path}
		if jsonMode {
			args = append(args, "--json")
		}
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "status", args, &out, &diag, deps); code != 0 {
			t.Fatalf("%d: %s", code, &diag)
		}
		s := out.String()
		for _, want := range []string{"tomorrow", "maintenance", "j_0123456789abcdef", "failed"} {
			if !strings.Contains(s, want) {
				t.Fatal(s)
			}
		}
		if strings.Contains(s, "sensitive") || strings.Contains(s, `"output"`) || (!jsonMode && !strings.Contains(s, "Next scheduled")) {
			t.Fatal(s)
		}
	}
}

func TestJobsRejectMalformedShow(t *testing.T) {
	for _, args := range [][]string{{"show"}, {"show", "../bad"}, {"show", "j_0123456789abcdef", "extra"}} {
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "jobs", args, &out, &diag, appDeps{}); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
}
