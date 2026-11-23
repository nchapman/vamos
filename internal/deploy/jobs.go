package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/jobresults"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// A transient worker receives identities, never a host command, shell string or container name.
type ScheduledJob struct{ App, Service, Name, HostID, Release, Incarnation string }

func (m *Manager) jobResults() jobresults.Store {
	return jobresults.Store{Root: m.path("/var/lib/vamos/job-results")}
}

func (job ScheduledJob) Unit() string {
	return "vamos-job-" + job.App + "." + job.Service + "." + job.Name + ".service"
}

func (m *Manager) jobHost() (*lifecycle.State, error) {
	path := m.path(lifecycle.StatePath)
	if err := inventoryPath(path, false, true); err != nil {
		return nil, err
	}
	if err := inventoryPath(filepath.Dir(path), true, false); err != nil {
		return nil, err
	}
	h, err := lifecycle.ReadState(path)
	if err != nil || h == nil || h.Phase != "ready" {
		return nil, errors.New("host is not ready for scheduled jobs")
	}
	return h, nil
}

func (m *Manager) jobJournal(app string, h *lifecycle.State) (*journal, error) {
	if err := inventoryPath(m.dir(app), true, true); err != nil {
		return nil, err
	}
	if err := inventoryPath(filepath.Join(m.dir(app), "state.json"), false, true); err != nil {
		return nil, err
	}
	j, err := m.load(app)
	if err != nil {
		return nil, errors.New("invalid scheduled job journal")
	}
	if j.HostID != h.ID || (j.Phase != "destroyed" && j.URL != "https://"+app+"."+h.Config.Domain) {
		return nil, errForeignApp
	}
	return j, nil
}

// Keep the last current definition visible even while dispatch is paused.
func currentJobRelease(j *journal, service string) (*release, string) {
	s := j.ServiceStates[service]
	if s == nil || s.Current == nil || s.Current.Config == nil {
		return nil, "no current release"
	}
	if j.Phase != "ready" || j.Maintenance != nil {
		return s.Current, "app is " + j.Phase
	}
	if s.Phase != "ready" || s.CurrentRetired || s.Unavailable {
		return s.Current, "service is unavailable"
	}
	return s.Current, ""
}

func due(s config.Schedule, minute time.Time) bool {
	compiled, err := s.Compile()
	return err == nil && compiled.Next(minute.Add(-time.Minute)).Equal(minute)
}

// DueJobs reads atomically published journals. Recheck the release when the worker starts.
func (m *Manager) DueJobs(ctx context.Context, tick time.Time) ([]ScheduledJob, error) {
	h, err := m.jobHost()
	if err != nil {
		return nil, err
	}
	apps, err := inventoryNames(m.path("/var/lib/vamos/apps"), true)
	if err != nil {
		return nil, err
	}
	minute := tick.Truncate(time.Minute)
	var jobs []ScheduledJob
	var errs []error
	for _, app := range apps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		j, err := m.jobJournal(app, h)
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: unable to read scheduled jobs", app))
			continue
		}
		for _, service := range j.ServiceOrder {
			r, _ := currentJobRelease(j, service)
			if r == nil {
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(r.Config.Jobs)) {
				if due(r.Config.Jobs[name].Schedule, minute) {
					jobs = append(jobs, ScheduledJob{app, service, name, h.ID, r.Operation, serviceIncarnation(j, service)})
				}
			}
		}
	}
	return jobs, errors.Join(errs...)
}

// ScheduledCommand resolves one current container via the same ownership, locks and
// immutable runtime identity checks as interactive exec. Never retry on a replacement.
func (m *Manager) ScheduledCommand(ctx context.Context, job ScheduledJob, minute time.Time) ([]string, error) {
	for _, name := range []string{job.App, job.Service, job.Name} {
		if !label.MatchString(name) || len(name) > 63 {
			return nil, errors.New("invalid scheduled job name")
		}
	}
	h, err := m.jobHost()
	if err != nil {
		return nil, err
	}
	if h.ID != job.HostID {
		return nil, errForeignApp
	}
	j, err := m.jobJournal(job.App, h)
	if err != nil {
		return nil, err
	}
	r, paused := currentJobRelease(j, job.Service)
	if paused != "" || r == nil || r.Operation != job.Release || serviceIncarnation(j, job.Service) != job.Incarnation {
		return nil, errors.New("scheduled release is no longer current")
	}
	def, ok := r.Config.Jobs[job.Name]
	if !ok || !due(def.Schedule, minute) {
		return nil, errors.New("job is not scheduled for this minute")
	}
	target, err := m.ResolveSession(ctx, SessionRequest{Version: 1, Host: h.Config, App: job.App, Service: job.Service, Replica: 1, Action: "exec", Command: def.Command})
	if err != nil {
		return nil, err
	}
	if target.Operation != r.Operation || target.HostIdentity != h.ID || target.Incarnation != job.Incarnation {
		return nil, errors.New("scheduled release changed during admission")
	}
	return append([]string{"exec", target.ContainerID}, def.Command...), nil
}

type JobStatus struct {
	Last     *jobresults.Run `json:"last,omitempty"`
	Paused   string          `json:"paused,omitempty"`
	Name     string          `json:"name"`
	Service  string          `json:"service"`
	Schedule string          `json:"schedule"`
	Next     string          `json:"next,omitempty"`
}

func (m *Manager) scheduledStatus(j *journal, selected string, now time.Time) ([]JobStatus, error) {
	var jobs []JobStatus
	for _, service := range j.ServiceOrder {
		if selected != "" && selected != service {
			continue
		}
		r, paused := currentJobRelease(j, service)
		if r == nil || len(r.Config.Jobs) == 0 {
			continue
		}
		attempts, err := m.jobResults().List(j.HostID, j.App, service, serviceIncarnation(j, service))
		if err != nil {
			return nil, err
		}
		for _, name := range slices.Sorted(maps.Keys(r.Config.Jobs)) {
			s := r.Config.Jobs[name].Schedule
			item := JobStatus{Name: name, Service: service, Schedule: s.Description(), Paused: paused}
			for _, attempt := range attempts {
				if attempt.Job == name {
					item.Last = &attempt
					break
				}
			}
			if compiled, err := s.Compile(); err == nil {
				if next := compiled.Next(now); !next.IsZero() {
					item.Next = next.Format(time.RFC3339)
				}
			}
			jobs = append(jobs, item)
		}
	}
	return jobs, nil
}
