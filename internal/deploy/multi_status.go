package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nchapman/soar/internal/secrets"
)

func multiSecretStatuses(state *secrets.State, j *journal) []SecretStatus {
	used := map[string]map[string]bool{}
	applied := map[string]map[string]string{}
	use := func(name, service string) {
		if used[name] == nil {
			used[name] = map[string]bool{}
		}
		used[name][service] = true
	}
	for service, cfg := range j.Config.Services {
		for _, name := range cfg.Secrets {
			use(name, service)
		}
	}
	for service, s := range j.ServiceStates {
		if s.Current == nil {
			continue
		}
		for _, ref := range s.Current.SecretRefs {
			use(ref.Name, service)
			if applied[ref.Name] == nil {
				applied[ref.Name] = map[string]string{}
			}
			applied[ref.Name][service] = ref.Version
		}
	}
	var result []SecretStatus
	for _, entry := range state.List() {
		if !entry.Latest {
			continue
		}
		s := SecretStatus{Name: entry.Name, StagedVersion: entry.Version, AppliedVersions: applied[entry.Name], Services: []string{}, Pending: len(used[entry.Name]) == 0}
		mixed := false
		for _, version := range s.AppliedVersions {
			if s.AppliedVersion == "" {
				s.AppliedVersion = version
			} else if s.AppliedVersion != version {
				mixed = true
			}
		}
		if mixed {
			s.AppliedVersion = ""
		}
		for service := range used[entry.Name] {
			s.Services = append(s.Services, service)
			if applied[entry.Name][service] != entry.Version {
				s.Pending = true
			}
		}
		sort.Strings(s.Services)
		result = append(result, s)
	}
	return result
}

func (m *Manager) multiStatus(ctx context.Context, j *journal, state *secrets.State, active bool, selected string) Status {
	out := maintenanceStatus(j, journalDomain(j))
	out.Release = m.releaseStatus(ctx, j)
	out.URL = ""
	out.Container = ""
	out.Active = active
	out.Running = true
	out.Secrets = multiSecretStatuses(state, j)
	if selected != "" {
		out.Secrets = slices.DeleteFunc(out.Secrets, func(s SecretStatus) bool { return !slices.Contains(s.Services, selected) })
		for i := range out.Secrets {
			s := &out.Secrets[i]
			s.AppliedVersion = s.AppliedVersions[selected]
			s.AppliedVersions = nil
			if s.AppliedVersion != "" {
				s.AppliedVersions = map[string]string{selected: s.AppliedVersion}
			}
			s.Services = []string{selected}
			s.Pending = s.AppliedVersion != s.StagedVersion
		}
	}
	out.Services = map[string]ServiceStatus{}
	for _, name := range j.ServiceOrder {
		if selected != "" && selected != name {
			continue
		}
		slot := j.ServiceStates[name]
		s := ServiceStatus{Kind: "private", Phase: slot.Phase, Unavailable: slot.Unavailable, DataMayHaveChanged: slot.DataMayHaveChanged}
		cfg := j.Config.Services[name]
		if cfg.HTTP != nil {
			s.Kind = "public"
			s.URL = ServiceURL(j.App, name, journalDomain(j))
		}
		if slot.Candidate != nil {
			s.Candidate = slot.Candidate.Container
		}
		if slot.Current != nil {
			r := slot.Current
			s.Container = r.Container
			s.Operation = r.Operation
			s.Image = r.Image
			s.Port = r.Port
			v, err := m.multiInspect(ctx, j, r, false, true)
			if err == nil {
				s.Running = v.State.Running
				s.Available = runtimeRunning(v) && !slot.CurrentRetired && !slot.Unavailable && j.Phase != "destroying" && v.HostConfig.RestartPolicy.Name == "unless-stopped"
			} else if j.Phase != "destroying" && out.LastError == "" {
				out.LastError = diagnostic(fmt.Sprintf("service %s: %s", name, err))
			}
			if name == "web" || selected == name {
				out.Container = r.Container
				out.URL = s.URL
			}
		}
		out.Running = out.Running && s.Running
		out.Services[name] = s
	}
	if len(out.Services) == 0 {
		out.Running = false
	}
	return out
}

func (m *Manager) readMulti(ctx context.Context, r Request, j *journal, state *secrets.State, active bool) (resp Response, err error) {
	resp.Version = 1
	if r.Action == "secrets-list" {
		resp.SecretVersions = state.List()
		return
	}
	if r.Service != "" && j.ServiceStates[r.Service] == nil {
		registry, e := m.volumeStore().Load(j.App, j.HostID)
		if e != nil {
			return resp, e
		}
		if r.Action != "status" || registry == nil || registry.Records[r.Service].ID == "" {
			return resp, errors.New("service has no recorded deployment")
		}
	}
	s := m.multiStatus(ctx, j, state, active, r.Service)
	resp.Status = &s
	if r.Action != "logs" {
		return
	}
	name := r.Service
	if name == "" {
		if j.ServiceStates["web"] != nil {
			name = "web"
		} else if len(j.ServiceStates) == 1 {
			for key := range j.ServiceStates {
				name = key
			}
		} else {
			return resp, errors.New("logs are ambiguous; select a service with --service")
		}
	}
	slot := j.ServiceStates[name]
	target := slot.Current
	if r.Candidate {
		target = slot.Candidate
	}
	if target == nil {
		return resp, errors.New("selected log target has no container; use --candidate for a pending replacement")
	}
	if _, err = m.multiInspect(ctx, j, target, r.Candidate && slot.Phase == "aliasing", !r.Candidate); err != nil {
		return
	}
	n := r.Tail
	if n == 0 {
		n = 100
	}
	resp.LogService = name
	resp.LogContainer = target.Container
	resp.LogCandidate = r.Candidate
	out, err := m.Run(ctx, "docker", []string{"logs", "--timestamps", "--tail", fmt.Sprint(n), target.Container}, nil)
	resp.Logs = diagnostic(string(out.Stdout) + string(out.Stderr))
	return resp, err
}

// Enrichment is response-only and reloads durable state after transitions.
func (m *Manager) enrichVolumeResponse(ctx context.Context, r Request, hostID string, resp *Response) {
	if resp.Status == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	latest, err := m.load(r.App)
	if errors.Is(err, os.ErrNotExist) {
		latest, err = nil, nil
	}
	var metadata []VolumeStatus
	if err == nil {
		metadata, err = m.volumeStatuses(ctx, r.App, hostID, latest, r.Service)
	}
	resp.Status.Volumes = metadata
	if err == nil {
		b, e := json.Marshal(resp)
		if e != nil || len(b) > 64<<10 {
			err = errors.New("volume status exceeds response limit; select --service")
		}
	}
	if err != nil {
		resp.Status.Volumes = nil
		message := "volume metadata inspection failed; ownership or runtime state could not be verified"
		if strings.Contains(err.Error(), "select --service") {
			message = "volume status exceeds response limit; select --service"
		}
		resp.Status.LastError = diagnostic(strings.TrimSpace(resp.Status.LastError + "\n" + message))
		// Metadata must not turn completed deployments or useful logs/status into
		// failed actions. Mark storage availability unverified and surface a warning.
		if latest != nil && latest.Config != nil {
			for name, service := range resp.Status.Services {
				if latest.Config.Services[name].Volume != nil {
					service.Available = false
					resp.Status.Services[name] = service
				}
			}
		}
	}
}
