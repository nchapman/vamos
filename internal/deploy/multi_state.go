package deploy

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/lifecycle"
)

func isMulti(j *journal) bool { return j != nil && (j.Version == 2 || j.Version == 3) }

func journalDomain(j *journal) string { return strings.TrimPrefix(j.URL, "https://"+j.App+".") }

func serviceOrder(j *journal) []string {
	names := make([]string, 0, len(j.ServiceStates))
	for name := range j.ServiceStates {
		names = append(names, name)
	}
	group := func(name string) int {
		if j.Config.Services[name].HTTP != nil {
			return 1
		}
		if j.ServiceStates[name].New {
			return 0
		}
		return 2
	}
	sort.Slice(names, func(i, k int) bool {
		a, b := group(names[i]), group(names[k])
		if a != b {
			return a < b
		}
		return names[i] < names[k]
	})
	return names
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == s
}

func validateServiceRelease(j *journal, name string, r *release, historical bool) error {
	bad := errors.New("invalid service release")
	if r == nil || !operation.MatchString(r.Operation) || !imageID.MatchString(r.Image) || !validSecretRefs(r.SecretRefs) {
		return bad
	}
	if r.Service != name && !(r.Service == "" && name == "web") {
		return bad
	}
	if r.Container != containerName(j.App, r.Service, r.Operation) {
		return bad
	}
	if r.Config == nil {
		if r.VolumeID != "" || r.ExecutionIntent {
			return bad
		}
		if !historical || r.Service != "" || !validPort(r.Port) {
			return bad
		}
	} else {
		a := &config.App{Name: j.App, Services: map[string]config.Service{name: *r.Config}}
		if ValidateApp(a) != nil || !matchingSecretRefs(r.SecretRefs, r.Config.Secrets) {
			return bad
		}
		if r.Config.Volume == nil {
			if r.VolumeID != "" || r.ExecutionIntent {
				return bad
			}
		} else if j.Version != 3 || r.Service == "" || !operation.MatchString(r.VolumeID) {
			return bad
		}
		if r.Config.HTTP == nil && (r.Port != "" || r.PublishPort != "") {
			return bad
		}
	}
	for _, port := range []string{r.Port, r.PublishPort} {
		if port != "" && !validPort(port) {
			return bad
		}
	}
	if r.Port != "" && r.PublishPort != "" && r.Port != r.PublishPort {
		return bad
	}
	if r.Service != "" && r.Port != "" && r.PublishPort == "" {
		return bad
	}
	return nil
}

func validateMultiJournal(j *journal, app string) error {
	bad := errors.New("invalid multi-service app journal")
	if !isMulti(j) || j.App != app || len(app) > 63 || !label.MatchString(app) || !operation.MatchString(j.Operation) || !operation.MatchString(j.HostID) || ValidateApp(j.Config) != nil || j.Config.Name != app || j.Archive != j.Operation+".context.tar" || !imageID.MatchString("sha256:"+j.BundleSHA) {
		return bad
	}
	if len(j.Volumes) != 0 || (j.Version != 3 && j.ReuseData) {
		return bad
	}
	if j.Current != nil || j.Previous != nil || j.Candidate != nil || len(j.SecretRefs) != 0 || j.Container != "" || len(j.Services) != 0 || len(j.Secrets) != 0 || j.Active || j.Running {
		return bad
	}
	if j.URL != "https://"+app+"."+journalDomain(j) || lifecycle.ValidateConfig(lifecycle.Config{Name: "host", Domain: journalDomain(j)}) != nil || len(strings.TrimPrefix(j.URL, "https://")) > 253 {
		return bad
	}
	if len(j.ServiceStates) != len(j.Config.Services) {
		return bad
	}
	builds := buildcontext.Builds(j.Config)
	for key, id := range j.BuildImages {
		if _, ok := builds[key]; !ok || !imageID.MatchString(id) {
			return bad
		}
	}
	if len(j.BuildImages) > len(builds) {
		return bad
	}
	switch j.Phase {
	case "building":
		if j.ActiveService != "" || j.NewCaddy != "" {
			return bad
		}
	case "updating", "ready", "cleaning", "destroying":
		if len(j.BuildImages) != len(builds) {
			return bad
		}
	default:
		return bad
	}
	if (j.OldCaddy == "") != (j.NewCaddy == "") {
		return bad
	}
	if j.ActiveService != "" && j.ServiceStates[j.ActiveService] == nil {
		return bad
	}
	if j.Phase != "updating" && j.ActiveService != "" {
		return bad
	}
	known := map[string]*release{}
	record := func(name string, r *release, historical bool) error {
		if err := validateServiceRelease(j, name, r, historical); err != nil {
			return err
		}
		if r.Config != nil && r.Config.Volume != nil {
			slot := j.ServiceStates[name]
			if slot == nil || slot.VolumeID != r.VolumeID || !reflect.DeepEqual(r.Config.Volume, j.Config.Services[name].Volume) {
				return bad
			}
		}
		if old := known[r.Container]; old != nil && !reflect.DeepEqual(old, r) {
			return errors.New("conflicting service release identities")
		}
		known[r.Container] = r
		return nil
	}
	for name, s := range j.ServiceStates {
		cfg, ok := j.Config.Services[name]
		if !ok || s == nil || !matchingSecretRefs(s.SecretRefs, cfg.Secrets) {
			return bad
		}
		if cfg.HTTP != nil && len(strings.TrimPrefix(ServiceURL(app, name, journalDomain(j)), "https://")) > 253 {
			return bad
		}
		if cfg.Volume == nil {
			if s.VolumeID != "" || s.Unavailable || s.DataMayHaveChanged {
				return bad
			}
		} else if j.Version != 3 || !operation.MatchString(s.VolumeID) {
			return bad
		}
		if s.Unavailable && cfg.HTTP == nil {
			return bad
		}
		if s.Current == nil && !s.New {
			return bad
		}
		if s.New && (s.CurrentRetired || (s.Current != nil && s.Current.Operation != j.Operation)) {
			return bad
		}
		if s.CurrentRetired && (s.Current == nil || (cfg.HTTP != nil && (cfg.Volume == nil || !s.Unavailable))) {
			return bad
		}
		for _, r := range []*release{s.Current, s.Candidate, s.Retiring} {
			if r != nil {
				if err := record(name, r, false); err != nil {
					return err
				}
				if r.VolumeID != s.VolumeID || !reflect.DeepEqual(r.Config.Volume, cfg.Volume) {
					return bad
				}
				if (r.Config.HTTP == nil) != (cfg.HTTP == nil) {
					return bad
				}
			}
		}
		if s.Current != nil && cfg.Volume != nil && !s.Current.ExecutionIntent {
			return bad
		}
		if s.Candidate != nil && s.Candidate.ExecutionIntent && !s.DataMayHaveChanged {
			return bad
		}
		if cfg.Volume != nil && (s.Phase == "aliasing" || s.Phase == "promoting") && (s.Candidate == nil || !s.Candidate.ExecutionIntent) {
			return bad
		}
		if s.Current != nil && s.Current.Config.HTTP != nil && s.Current.Port == "" {
			return bad
		}
		if s.Candidate != nil {
			if s.Candidate.Operation != j.Operation || s.Candidate.Service != name || !reflect.DeepEqual(*s.Candidate.Config, cfg) || !maps.Equal(s.Candidate.SecretRefs, s.SecretRefs) || s.Candidate.Image != j.BuildImages[buildcontext.BuildKey(*cfg.Build)] {
				return bad
			}
			if s.Current != nil && s.Current.Container == s.Candidate.Container {
				return bad
			}
		}
		if s.Current != nil && s.Current.Operation == j.Operation {
			if s.Current.Service != name || !reflect.DeepEqual(*s.Current.Config, cfg) || !maps.Equal(s.Current.SecretRefs, s.SecretRefs) || s.Current.Image != j.BuildImages[buildcontext.BuildKey(*cfg.Build)] {
				return bad
			}
		}
		if s.Retiring != nil && (s.Phase != "draining" || s.Current == nil || s.Retiring.Container == s.Current.Container || cfg.HTTP == nil) {
			return bad
		}
		switch s.Phase {
		case "pending":
			if j.Phase != "building" || s.Candidate != nil || s.Retiring != nil {
				return bad
			}
		case "prepared":
			if s.Candidate == nil || s.Retiring != nil {
				return bad
			}
		case "withdrawing":
			if j.Version != 3 || cfg.Volume == nil || cfg.HTTP == nil || s.Candidate == nil || s.Retiring != nil || s.Unavailable {
				return bad
			}
		case "retiring":
			if (cfg.HTTP != nil && (cfg.Volume == nil || !s.Unavailable)) || s.Candidate == nil || !s.CurrentRetired || s.Retiring != nil {
				return bad
			}
		case "starting":
			if s.Candidate == nil || s.Retiring != nil || ((cfg.HTTP == nil || cfg.Volume != nil) && s.Current != nil && !s.CurrentRetired) || (cfg.Volume != nil && cfg.HTTP != nil && !s.Unavailable) {
				return bad
			}
		case "aliasing", "promoting":
			if cfg.HTTP == nil || s.Candidate == nil || s.Retiring != nil || (cfg.Volume != nil && !s.Unavailable) {
				return bad
			}
		case "draining":
			if cfg.HTTP == nil || s.Current == nil || s.Current.Operation != j.Operation || s.Candidate != nil {
				return bad
			}
		case "ready":
			if s.Current == nil || s.Candidate != nil || s.Retiring != nil || s.CurrentRetired || s.Unavailable {
				return bad
			}
		default:
			return bad
		}
		if j.Phase == "building" && s.Phase != "pending" {
			return bad
		}
		if (j.Phase == "ready" || j.Phase == "cleaning" || j.Phase == "destroying") && s.Phase != "ready" {
			return bad
		}
	}
	if !slices.Equal(j.ServiceOrder, serviceOrder(j)) {
		return bad
	}
	if j.NewCaddy != "" {
		s := j.ServiceStates[j.ActiveService]
		if j.Phase != "updating" || s == nil || (s.Phase != "promoting" && s.Phase != "withdrawing") || s.Candidate == nil || s.Candidate.Config.HTTP == nil {
			return bad
		}
	}
	for _, snapshot := range []*completedRelease{j.Completed, j.PreviousCompleted} {
		if snapshot == nil {
			continue
		}
		if !operation.MatchString(snapshot.Operation) || len(snapshot.Services) == 0 || len(snapshot.Services) > MaxServices {
			return bad
		}
		for name, r := range snapshot.Services {
			if j.ServiceStates[name] == nil {
				return bad
			}
			if err := record(name, r, true); err != nil {
				return err
			}
			if r.Config != nil && r.Config.HTTP != nil && r.Port == "" {
				return bad
			}
		}
	}
	if j.PreviousCompleted != nil && j.Completed == nil {
		return bad
	}
	if (j.Phase == "ready" || j.Phase == "cleaning" || j.Phase == "destroying") && (j.Completed == nil || !sameEffective(j)) {
		return bad
	}
	return validateMaintenance(j)
}

func sameEffective(j *journal) bool {
	if j.Completed == nil || len(j.Completed.Services) != len(j.ServiceStates) {
		return false
	}
	for name, s := range j.ServiceStates {
		if s.CurrentRetired || !reflect.DeepEqual(j.Completed.Services[name], s.Current) {
			return false
		}
	}
	return true
}

func checkTransitions(old *journal, a *config.App) error {
	if old == nil || old.Phase == "destroyed" {
		return nil
	}
	if old.Version == 1 {
		if old.Phase != "ready" {
			return errors.New("finish the legacy deployment with resume before upgrading to multi-service deployment")
		}
		if s, ok := a.Services["web"]; !ok || s.HTTP == nil || s.Volume != nil {
			return errors.New("removing web or changing its public/private kind is not supported")
		}
		return nil
	}
	for name, previous := range old.Config.Services {
		next, exists := a.Services[name]
		if (exists && !reflect.DeepEqual(previous.Volume, next.Volume)) || (!exists && previous.Volume != nil) {
			return fmt.Errorf("service %s: volume bindings are immutable; adding, removing, or changing storage requires a new service", name)
		}
	}
	for name, s := range old.ServiceStates {
		if s.Current == nil {
			continue
		}
		cfg, ok := a.Services[name]
		if !ok {
			return fmt.Errorf("service %s: removing an effective service is not supported; omission never deletes it", name)
		}
		if (cfg.HTTP == nil) != (s.Current.Config.HTTP == nil) {
			return fmt.Errorf("service %s: public/private transitions are not supported", name)
		}
	}
	return nil
}
