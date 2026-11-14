package deploy

import (
	"context"
	"regexp"

	"github.com/nchapman/soar/internal/secrets"
)

func (m *Manager) secretStore() secrets.Store {
	return secrets.Store{Dir: m.path("/var/lib/soar/secrets")}
}

func secretOnlyStatus(app, domain string) Status {
	return Status{App: app, URL: "https://" + app + "." + domain, Phase: "staged"}
}

func secretStatuses(state *secrets.State, j *journal) []SecretStatus {
	if j != nil && isMulti(j) {
		return multiSecretStatuses(state, j)
	}
	applied := map[string]string{}
	used := map[string]bool{}
	if j != nil {
		if j.Config != nil {
			for _, name := range j.Config.Services["web"].Secrets {
				used[name] = true
			}
		}
		if j.Current != nil {
			for _, ref := range j.Current.SecretRefs {
				applied[ref.Name] = ref.Version
				used[ref.Name] = true
			}
		}
	}
	var result []SecretStatus
	for _, entry := range state.List() {
		if !entry.Latest {
			continue
		}
		s := SecretStatus{Name: entry.Name, StagedVersion: entry.Version, AppliedVersion: applied[entry.Name], Pending: entry.Version != applied[entry.Name], Services: []string{}}
		if used[entry.Name] {
			s.Services = []string{"web"}
		}
		result = append(result, s)
	}
	return result
}

var secretEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validSecretRefs(refs map[string]secrets.Ref) bool {
	// Keep environment mapping metadata bounded independently of value storage.
	if len(refs) > secrets.MaxNames {
		return false
	}
	versions := map[string]string{}
	for key, ref := range refs {
		if !secretEnvKey.MatchString(key) || !secrets.ValidName(ref.Name) || !secrets.ValidVersion(ref.Version) {
			return false
		}
		if previous := versions[ref.Name]; previous != "" && previous != ref.Version {
			return false
		}
		versions[ref.Name] = ref.Version
	}
	return true
}

func matchingSecretRefs(refs map[string]secrets.Ref, mapping map[string]string) bool {
	if !validSecretRefs(refs) || len(refs) != len(mapping) {
		return false
	}
	for key, name := range mapping {
		if refs[key].Name != name {
			return false
		}
	}
	return true
}

func (m *Manager) secretValues(j *journal, refs map[string]secrets.Ref) (map[string]string, error) {
	state, err := m.secretStore().Load(j.App, j.HostID)
	if err != nil {
		return nil, err
	}
	return state.Values(refs)
}

// Docker reads literal values from a private worker file, not argv or its own
// process environment. The worker wrapper also removes the file on exit.
func (m *Manager) runContainer(ctx context.Context, j *journal, r *release, args []string) error {
	values, err := m.secretValues(j, r.SecretRefs)
	if err != nil {
		return err
	}
	return m.engine().RunWithEnvFile(ctx, r.Image, args, values)
}
