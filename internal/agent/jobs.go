package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// Wake on the next wall-clock minute, not a minute after deploy or agent startup.
// Never catch up missed work. Stable transient unit names prevent overlapping
// ticks for a job, including across agent restarts, without a second job store.
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
			if err := startScheduledJob(tick, job, minute); err != nil {
				log.Printf("scheduled job %s/%s/%s not started (busy or unavailable)", job.App, job.Service, job.Name)
			}
		}
		cancel()
	}
}

func startScheduledJob(ctx context.Context, job deploy.ScheduledJob, minute time.Time) error {
	// Never pass user argv through systemd's environment/specifier expansion.
	return exec.CommandContext(ctx, "systemd-run", "--quiet", "--no-block", "--collect", "--unit="+job.Unit(),
		lifecycle.ExecutorPath, "internal-scheduled-job", job.App, job.Service, job.Name, job.HostID, job.Release, strconv.FormatInt(minute.Unix(), 10)).Run()
}

func RunScheduledJob(ctx context.Context, args []string) error {
	if len(args) != 6 {
		return errors.New("invalid scheduled job arguments")
	}
	stamp, err := strconv.ParseInt(args[5], 10, 64)
	if err != nil {
		return errors.New("invalid scheduled minute")
	}
	minute := time.Unix(stamp, 0)
	end := minute.Add(time.Minute)
	if !time.Now().Truncate(time.Minute).Equal(minute) {
		return errors.New("scheduled minute has passed")
	}
	admit, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	job := deploy.ScheduledJob{App: args[0], Service: args[1], Name: args[2], HostID: args[3], Release: args[4]}
	argv, err := deploy.NewManager().ScheduledCommand(admit, job, minute)
	if err != nil || admit.Err() != nil {
		return errors.New("scheduled job is no longer eligible or its container is unavailable")
	}
	// Output is deliberately not copied into root's journal; it can contain app secrets.
	if err := exec.CommandContext(ctx, "docker", argv...).Run(); err != nil {
		return fmt.Errorf("scheduled container command: %w", err)
	}
	return nil
}
