package backup

import (
	"errors"
	"regexp"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// PlanRequest never carries application or repository credential values.
type PlanRequest struct {
	Action           string `json:"action"`
	Plan             *Plan  `json:"plan,omitempty"`
	ExpectedRevision string `json:"expected_revision,omitempty"`
	ExpectedDigest   string `json:"expected_digest,omitempty"`
}

func ValidatePlanRequest(app string, r PlanRequest) error {
	if !ValidName(app) {
		return errors.New("invalid backup plan app")
	}
	switch r.Action {
	case "show":
		if r.Plan != nil || r.ExpectedRevision != "" || r.ExpectedDigest != "" {
			return errors.New("backup plan observation accepts only an app")
		}
	case "preview", "apply":
		if r.Plan == nil || r.Plan.App != app || ValidatePlan(*r.Plan) != nil {
			return errors.New("invalid backup plan")
		}
		if r.Action == "preview" {
			if r.ExpectedRevision != "" || r.ExpectedDigest != "" {
				return errors.New("backup plan preview accepts no approval fields")
			}
		} else if !digestPattern.MatchString(r.ExpectedDigest) || (r.ExpectedRevision != "none" && !digestPattern.MatchString(r.ExpectedRevision)) {
			return errors.New("backup plan apply requires the exact preview revision and digest")
		}
	default:
		return errors.New("unsupported backup plan action")
	}
	return nil
}

// PlanRuntime is supplied by the deployment owner after inspecting stable,
// effective runtimes. It intentionally contains neither desired configuration
// nor secret values/pins: every eventual capture must resolve applied pins anew.
type PlanRuntime struct {
	HostID   string
	App      string
	Services map[string]PlanService
}
type PlanService struct {
	Incarnation string
	Replicas    int
	Volume      *PlanVolume
	SecretEnvs  []string
}
type PlanVolume struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Mount string `json:"mount"`
}

// Incarnations are service-scoped: services created together may share the
// deployment operation token. Always bind the service name as well as its token.
type SourceBinding struct {
	Service               string      `json:"service"`
	Incarnation           string      `json:"incarnation"`
	Volume                *PlanVolume `json:"volume,omitempty"`
	CredentialService     string      `json:"credential_service,omitempty"`
	CredentialIncarnation string      `json:"credential_incarnation,omitempty"`
}
type RepositoryPin struct {
	ID           string `json:"id"`
	ConfigDigest string `json:"config_digest"`
}
type PlanBindings struct {
	Sources      map[string]SourceBinding `json:"sources"`
	Stops        map[string]string        `json:"stops"`
	Repositories map[string]RepositoryPin `json:"repositories"`
}
type PlanRecord struct {
	Version   int          `json:"version"`
	HostID    string       `json:"host_id"`
	Revision  string       `json:"revision"`
	Plan      Plan         `json:"plan"`
	Bindings  PlanBindings `json:"bindings"`
	UpdatedAt string       `json:"updated_at"`
}
type PlanResponse struct {
	// Current is the installed plan, not an assertion of current capture readiness.
	Current         *PlanRecord `json:"current,omitempty"`
	CurrentRevision string      `json:"current_revision"`
	Proposed        *PlanRecord `json:"proposed,omitempty"`
	Digest          string      `json:"digest,omitempty"`
	Applied         bool        `json:"applied"`
	Warnings        []string    `json:"warnings"`
}
