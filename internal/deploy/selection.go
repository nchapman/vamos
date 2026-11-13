package deploy

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/secrets"
)

// A scoped operation captures actual unselected configurations, never local
// pending edits. Undeployed declarations aren't effective services yet.
func effectiveConfig(r Request, old *journal) (*config.App, error) {
	if r.Service == "" && !r.Partial {
		return r.Config, nil
	}
	a := &config.App{Name: r.App, Services: map[string]config.Service{}}
	for _, name := range selectedServices(r) {
		cfg, ok := r.Config.Services[name]
		if !ok {
			return nil, fmt.Errorf("unknown service %s", name)
		}
		a.Services[name] = cfg
	}
	if old != nil && old.Phase != "destroyed" {
		if old.Version == 1 {
			if _, selected := a.Services["web"]; !selected {
				a.Services["web"] = old.Config.Services["web"]
			}
		} else {
			for name, s := range old.ServiceStates {
				if _, selected := a.Services[name]; selected {
					continue
				}
				if s.Current == nil || s.Current.Config == nil || s.CurrentRetired {
					return nil, errors.New("unselected service has incomplete deployment; resume or supersede with the original service scope")
				}
				a.Services[name] = *s.Current.Config
			}
		}
	}
	if err := ValidateApp(a); err != nil {
		return nil, err
	}
	return a, nil
}

func workflowConfig(a *config.App) bool {
	for _, cfg := range a.Services {
		if cfg.Image != "" || cfg.Update == config.UpdateManual {
			return true
		}
	}
	return false
}

func supportsVolumes(j *journal) bool {
	return j != nil && (j.Version == 3 || j.Version == 4 || j.Version == 5 || j.Version == 6 || j.Version == 7)
}

func protectedChange(name string) error {
	return fmt.Errorf("service %s is protected and its configuration or staged secrets changed; explicitly deploy it with soar deploy --service %s --yes", name, name)
}

func keepService(r Request, name string, cfg config.Service, current *release, state *secrets.State) (bool, error) {
	if r.Service != "" {
		return name != r.Service, nil
	}
	if r.Partial {
		if _, selected := r.Config.Services[name]; !selected {
			return true, nil
		}
	}
	if current == nil || current.Config == nil {
		return false, nil
	}
	// Removing manual protection is itself a protected configuration change.
	if current.Config.Update != config.UpdateManual && cfg.Update != config.UpdateManual {
		return false, nil
	}
	if !reflect.DeepEqual(*current.Config, cfg) {
		return false, protectedChange(name)
	}
	refs, err := state.Snapshot(cfg.Secrets)
	if err != nil || !maps.Equal(refs, current.SecretRefs) {
		return false, protectedChange(name)
	}
	return true, nil
}

// The bundle captures selected local sources; the journal also contains
// unselected effective services, which must not require local build contexts.
func bundleConfig(j *journal) *config.App {
	if j.Version == 7 {
		a := &config.App{Name: j.App, Services: map[string]config.Service{}}
		for _, name := range j.Selected {
			a.Services[name] = j.Config.Services[name]
		}
		return a
	}
	if j.Scope == "" {
		return j.Config
	}
	return &config.App{Name: j.App, Services: map[string]config.Service{j.Scope: j.Config.Services[j.Scope]}}
}

func activeSources(j *journal) map[string]config.Service {
	sources := map[string]config.Service{}
	for name, cfg := range j.Config.Services {
		if s := j.ServiceStates[name]; s != nil && s.Skipped {
			continue
		}
		sources[imageKey(cfg)] = cfg
	}
	return sources
}

func deploymentPreview(r Request, old *journal) *DeploymentPreview {
	if r.Service == "" || old == nil || old.Phase == "destroyed" {
		return nil
	}
	var current *release
	if old.Version == 1 && r.Service == "web" {
		current = old.Current
	} else if s := old.ServiceStates[r.Service]; s != nil {
		current = s.Current
	}
	if current == nil {
		return nil
	}
	cfg := r.Config.Services[r.Service]
	prior := current.Config
	if prior == nil {
		legacy := old.Config.Services[r.Service]
		prior = &legacy
	}
	if prior.Update != config.UpdateManual && cfg.Update != config.UpdateManual {
		return nil
	}
	desired := cfg.Image
	if desired == "" {
		desired = "captured Dockerfile build"
	}
	return &DeploymentPreview{Service: r.Service, CurrentImage: current.Image, DesiredImage: desired, Downtime: cfg.HTTP == nil || cfg.Volume != nil, NeedsConfirmation: !r.Apply}
}

// selectedServices captures the local manifest before host-side merging.
func selectedServices(r Request) []string {
	if r.Service != "" {
		return []string{r.Service}
	}
	names := make([]string, 0, len(r.Config.Services))
	for name := range r.Config.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func capturedSelection(j *journal) []string {
	if j.Version == 7 {
		return j.Selected
	}
	return selectedServices(Request{Service: j.Scope, Config: j.Config})
}
