package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/jobresults"
)

func jobsArgs(args []string) (string, []string, error) {
	if len(args) == 0 || args[0] != "show" {
		return "", args, nil
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		return "", args[1:], nil
	}
	if len(args) < 2 || !jobresults.ValidID(args[1]) {
		return "", nil, errors.New("jobs show requires a valid run ID")
	}
	return args[1], args[2:], nil
}

func jobField(p *presentation, label, value string) string {
	if p != nil {
		return p.field(label, value)
	}
	return label + ": " + displayText(value) + "\n"
}
func jobSummary(r jobresults.Run) string {
	detail := r.Outcome
	if r.ExitCode != nil && *r.ExitCode != 0 {
		detail += fmt.Sprintf(" (exit %d)", *r.ExitCode)
	} else if r.Started != nil && r.Finished != nil {
		detail += " (" + r.Finished.Sub(*r.Started).Round(time.Millisecond).String() + ")"
	}
	return fmt.Sprintf("%s  %s  %s / %s  %s", r.ID, r.Scheduled.Format(time.RFC3339), r.Service, r.Job, detail)
}
func jobStatusFields(p *presentation, j deploy.JobStatus) string {
	var s string
	if j.Paused != "" {
		s += jobField(p, "Paused", j.Paused)
	}
	if j.Last != nil {
		s += jobField(p, "Last run", jobSummary(*j.Last))
		if j.Last.Reason != "" {
			s += jobField(p, "Reason", j.Last.Reason)
		}
	}
	return s
}

// Override the transport's byte slice: detail JSON is text, and summaries never contain logs.
type jobView struct {
	jobresults.Run
	Output *string `json:"output,omitempty"`
}

func writeJobs(w io.Writer, p *presentation, target appReport, response deploy.Response, id string, asJSON bool) error {
	var b strings.Builder
	if p != nil {
		b.WriteString(p.header("Jobs", target.app, target.server.Name, target.server.Target))
	}
	if id == "" {
		if asJSON {
			runs := make([]jobView, 0, len(response.JobRuns))
			for _, r := range response.JobRuns {
				runs = append(runs, jobView{Run: r})
			}
			return json.NewEncoder(w).Encode(runs)
		}
		if len(response.JobRuns) == 0 {
			b.WriteString("No retained scheduled job runs.\n")
		}
		if len(response.JobRuns) > 0 && p != nil {
			b.WriteString(p.section("Recent runs · newest first"))
		}
		for _, r := range response.JobRuns {
			b.WriteString(displayText(jobSummary(r)) + "\n")
		}
		if len(response.JobRuns) > 0 && p != nil {
			b.WriteString(p.prose("Inspect a run with vamos jobs show <id>."))
		}
	} else {
		r := response.JobRun
		if r == nil || r.ID != id {
			return errors.New("remote response missing requested job run")
		}
		output := string(r.Output)
		if asJSON {
			return json.NewEncoder(w).Encode(jobView{Run: *r, Output: &output})
		}
		field := func(k, v string) { b.WriteString(jobField(p, k, v)) }
		field("ID", r.ID)
		field("Service", r.Service)
		field("Job", r.Job)
		field("Release", r.Release)
		field("Outcome", r.Outcome)
		if r.Reason != "" {
			field("Reason", r.Reason)
		}
		field("Scheduled", r.Scheduled.Format(time.RFC3339))
		if r.Started != nil {
			field("Started", r.Started.Format(time.RFC3339))
		}
		if r.Finished != nil {
			field("Finished", r.Finished.Format(time.RFC3339))
		}
		if r.ExitCode != nil {
			field("Exit code", strconv.Itoa(*r.ExitCode))
		}
		if len(output) > 0 || r.Truncated {
			b.WriteString("Combined output:\n")
		} else if r.Outcome != "skipped" {
			b.WriteString("No captured output.\n")
		}
		if r.Truncated {
			b.WriteString("[earlier output truncated]\n")
		}
		// Only LF is structural. Preserve leading spaces and render all other controls visibly.
		for len(output) > 0 {
			line, rest, _ := strings.Cut(output, "\n")
			b.WriteString(displayText(strings.ReplaceAll(line, "\t", "    ")))
			b.WriteByte('\n')
			output = rest
		}
	}
	if p != nil {
		return p.write(w, b.String())
	}
	_, err := io.WriteString(w, b.String())
	return err
}
