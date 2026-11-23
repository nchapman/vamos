package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/jobresults"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// BeginScheduledAttempt records an observed tick before handing it to systemd.
// Shared host locks fence removal and teardown, but don't contend with sibling
// job admissions. Never recreate a purged service's inbox from a stale scan.
func (m *Manager) BeginScheduledAttempt(job ScheduledJob, minute time.Time) (jobresults.Run, error) {
	var run jobresults.Run
	for _, path := range []string{m.path(lifecycle.LockPath), m.path("/run/lock/vamos/host-op.lock")} {
		if err := safe(path); err != nil {
			return run, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return run, err
		}
		lock := flock.New(path, flock.SetPermissions(0600))
		defer lock.Close()
		ok, err := lock.TryRLock()
		if err != nil {
			return run, err
		}
		if !ok {
			return run, errors.New("host maintenance in progress")
		}
	}
	h, err := m.jobHost()
	if err != nil {
		return run, err
	}
	if h.ID != job.HostID {
		return run, errForeignApp
	}
	j, err := m.jobJournal(job.App, h)
	if err != nil {
		return run, err
	}
	r, paused := currentJobRelease(j, job.Service)
	if r == nil || r.Operation != job.Release || serviceIncarnation(j, job.Service) != job.Incarnation {
		return run, errors.New("scheduled service changed")
	}
	if j.Maintenance != nil && (j.Maintenance.Action == "destroy" || (j.Maintenance.Action == "service-remove" && j.Maintenance.Service == job.Service)) {
		return run, errors.New("scheduled service is being removed")
	}
	def, ok := r.Config.Jobs[job.Name]
	if !ok || !due(def.Schedule, minute) {
		return run, errors.New("job no longer due")
	}
	id, err := jobresults.NewID()
	if err != nil {
		return run, err
	}
	run = jobresults.Run{ID: id, Service: job.Service, Job: job.Name, Release: job.Release, Scheduled: minute, Outcome: "unknown", Reason: "worker start has not been confirmed"}
	if paused != "" {
		now := time.Now()
		run.Outcome, run.Reason, run.Finished = "skipped", paused, &now
	}
	return run, m.jobResults().Create(job.HostID, job.App, job.Incarnation, run)
}

func (m *Manager) UpdateScheduledAttempt(job ScheduledJob, run jobresults.Run) error {
	return m.jobResults().Finish(job.HostID, job.App, job.Incarnation, run)
}

// Snapshot known app secrets before execution so even a concurrent deploy or
// secret rotation doesn't discard the redaction context of this run.
func (m *Manager) ScheduledOutputRedactor(job ScheduledJob) (func(string) string, error) {
	state, err := m.secretStore().Load(job.App, job.HostID)
	if err != nil {
		return nil, err
	}
	return state.Redact, nil
}
