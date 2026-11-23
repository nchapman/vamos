package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nchapman/vamos/internal/jobresults"
	"log"
	"os/exec"
	"strconv"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// Wake on the next wall-clock minute, not a minute after deploy or agent startup.
// Never catch up missed work. Stable transient unit names prevent overlapping
// ticks for a job, including across agent restarts. History is observational, never a queue.
func runJobClock(ctx context.Context) {
	manager := deploy.NewManager()
	for ctx.Err() == nil {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		minute := time.Now().Truncate(time.Minute)
		tick, cancel := context.WithDeadline(ctx, minute.Add(time.Minute))
		jobs, err := manager.DueJobs(tick, minute)
		if err != nil {
			log.Printf("scheduled jobs: %v", err)
		}
		for _, job := range jobs {
			if tick.Err() != nil {
				break
			}
			if err := dispatchScheduledJob(tick, manager, job, minute); err != nil {
				log.Printf("scheduled job %s/%s/%s: %v", job.App, job.Service, job.Name, err)
			}
		}
		cancel()
	}
}

func dispatchScheduledJob(ctx context.Context, m *deploy.Manager, job deploy.ScheduledJob, minute time.Time) error {
	run, err := m.BeginScheduledAttempt(job, minute)
	if err != nil || run.Outcome == "skipped" {
		return err
	}
	// Never pass app argv through systemd's environment/specifier expansion.
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--no-block", "--collect", "--unit="+job.Unit(), lifecycle.ExecutorPath, "internal-scheduled-job", job.App, job.Service, job.Name, job.HostID, job.Release, job.Incarnation, run.ID, strconv.FormatInt(minute.Unix(), 10))
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// A lost acknowledgement is not proof of refusal; leave unknown rather
	// than overwrite a late result from a worker that actually started.
	if reason := launchRefusal(cmd.Process != nil, job.Unit(), string(out)); reason != "" {
		return endAdmission(m, job, run, reason)
	}
	return fmt.Errorf("%s: systemd launch not confirmed: %w", run.ID, err)
}

// Once the launcher has contacted systemd, a generic nonzero exit cannot
// distinguish a refused request from a lost reply. Only record known refusals.
func launchRefusal(launcherStarted bool, unit, diagnostic string) string {
	if !launcherStarted {
		return "could not start systemd-run"
	}
	if strings.HasPrefix(diagnostic, "Failed to connect to bus:") {
		return "could not connect to systemd"
	}
	if strings.Contains(diagnostic, unit) && (strings.Contains(diagnostic, "already loaded") || strings.Contains(diagnostic, "already exists")) {
		return "previous job unit still active or being collected"
	}
	return ""
}

func endAdmission(m *deploy.Manager, job deploy.ScheduledJob, run jobresults.Run, reason string) error {
	now := time.Now()
	run.Outcome, run.Reason, run.Finished = "skipped", reason, &now
	return m.UpdateScheduledAttempt(job, run)
}

func RunScheduledJob(ctx context.Context, args []string) error {
	if len(args) != 8 || !jobresults.ValidID(args[6]) {
		return errors.New("invalid scheduled job arguments")
	}
	stamp, err := strconv.ParseInt(args[7], 10, 64)
	if err != nil {
		return errors.New("invalid scheduled minute")
	}
	minute := time.Unix(stamp, 0)
	job := deploy.ScheduledJob{App: args[0], Service: args[1], Name: args[2], HostID: args[3], Release: args[4], Incarnation: args[5]}
	run := jobresults.Run{ID: args[6], Service: job.Service, Job: job.Name, Release: job.Release, Scheduled: minute, Outcome: "unknown", Reason: "completion has not been observed"}
	m := deploy.NewManager()
	end := minute.Add(time.Minute)
	if !time.Now().Truncate(time.Minute).Equal(minute) {
		return endAdmission(m, job, run, "scheduled minute has passed")
	}
	admit, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	argv, err := m.ScheduledCommand(admit, job, minute)
	if err != nil || admit.Err() != nil {
		return endAdmission(m, job, run, "release changed, a mutation was active, or the container was unavailable")
	}
	redact, err := m.ScheduledOutputRedactor(job)
	if err != nil {
		return endAdmission(m, job, run, "could not prepare output redaction")
	}
	start := time.Now()
	run.Started = &start
	if err = m.UpdateScheduledAttempt(job, run); err != nil {
		return err
	}
	if !time.Now().Before(end) {
		run.Started = nil
		return endAdmission(m, job, run, "scheduled minute has passed")
	}
	err = observeCommand(ctx, exec.CommandContext(ctx, "docker", argv...), &run, redact)
	return errors.Join(err, m.UpdateScheduledAttempt(job, run))
}
