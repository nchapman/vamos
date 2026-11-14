package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/nchapman/soar/internal/backupbackend"
)

var repositoryPasswordPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var (
	errInput     = errors.New("invalid repository request or private credentials")
	errConflict  = errors.New("repository name or operation conflicts with retained configuration")
	errMissing   = errors.New("repository is not registered")
	errCapacity  = errors.New("repository registration limit reached; no state was pruned")
	errPreflight = errors.New("repository preflight failed; no initialization was attempted")
	errUnknown   = errors.New("repository initialization outcome unknown; resume the captured operation without reinitializing")
	errVerify    = errors.New("repository authentication, format, or identity verification failed")
	errResume    = errors.New("repository initialization must be reconciled with its original operation")
)

var refusalMessages = map[string]error{
	"backup_plan_input": errPlanInput, "backup_plan_state": errPlanState,
	"backup_plan_conflict": errPlanConflict, "backup_plan_selection": errPlanSelection,
	"backup_plan_repository": errPlanRepository,
	"backup_input":           errInput, "backup_state": errStore, "backup_conflict": errConflict,
	"backup_missing": errMissing, "backup_capacity": errCapacity, "backup_preflight": errPreflight,
	"backup_unknown": errUnknown, "backup_verify": errVerify, "backup_resume": errResume,
}

func ErrorCode(err error) string {
	for code, value := range refusalMessages {
		if errors.Is(err, value) {
			return code
		}
	}
	return ""
}
func RefusalMessage(code string) string {
	if err := refusalMessages[code]; err != nil {
		return err.Error()
	}
	return ""
}

type Backend interface {
	Probe(context.Context, backupbackend.Config, backupbackend.Credentials) (backupbackend.Identity, error)
	CheckEmpty(context.Context, backupbackend.Config, backupbackend.Credentials) error
	Init(context.Context, backupbackend.Config, backupbackend.Credentials) (backupbackend.Identity, error)
}

type Manager struct {
	Root       string
	RuntimeDir string
	Backend    Backend
	Now        func() time.Time
}

func NewManager(root, runtimeDir string) *Manager {
	return &Manager{Root: root, RuntimeDir: runtimeDir, Backend: backupbackend.New(root, runtimeDir)}
}
func (m *Manager) storage() store {
	return store{dir: filepath.Join(m.Root, "var/lib/soar/backups/repositories")}
}
func (m *Manager) timestamp() string {
	if m.Now != nil {
		return m.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Execute requires a verified ready host identity and, for Mutating requests,
// the caller's existing global host mutation lock. No application journal is read
// or changed. Private inputs never enter a Response, diagnostic, or request JSON.
func (m *Manager) Execute(ctx context.Context, hostID string, r Request, privateInput []byte) (Response, error) {
	var response Response
	if ValidateRequest(r) != nil || !ValidOperation(hostID) || ctx.Err() != nil {
		return response, errInput
	}
	var credentials backupbackend.Credentials
	if r.NeedsCredentials() {
		var err error
		credentials, err = DecodeCredentials(privateInput)
		if err != nil {
			return response, errInput
		}
	} else if len(privateInput) != 0 {
		return response, errInput
	}
	if r.Action == "create" && !repositoryPasswordPattern.MatchString(credentials.Password) {
		return response, errInput
	}
	s := m.storage()
	if r.Action == "list" {
		records, err := s.list(hostID)
		if err != nil {
			return response, err
		}
		response.Repositories = []Metadata{}
		for _, record := range records {
			response.Repositories = append(response.Repositories, record.Metadata)
		}
		return response, nil
	}
	if r.Action == "probe" {
		if m.Backend == nil {
			return response, errVerify
		}
		identity, err := m.Backend.Probe(ctx, *r.Config, credentials)
		if err != nil || backupbackend.ValidateIdentity(identity) != nil {
			return response, errVerify
		}
		response.Repository = &Metadata{Name: r.Name, Mode: "probe", State: "observed", Config: *r.Config, RepositoryID: identity.ID, Format: identity.Version, VerifiedAt: m.timestamp()}
		return response, nil
	}
	existing, err := s.load(r.Name, hostID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return response, err
	}
	if r.Action == "status" {
		if existing == nil {
			return response, errMissing
		}
		response.Repository = &existing.Metadata
		return response, nil
	}
	if r.Action == "create" || r.Action == "connect" {
		if existing != nil {
			if existing.Metadata.Operation != r.Operation || existing.Metadata.Mode != r.Action || existing.Metadata.Config != *r.Config || existing.Credentials != credentials || (r.Action == "connect" && existing.Metadata.RepositoryID != r.ExpectedID) {
				return response, errConflict
			}
		} else {
			records, listErr := s.list(hostID)
			if listErr != nil {
				return response, listErr
			}
			if len(records) >= MaxRepositories {
				return response, errCapacity
			}
			existing = &record{Version: 1, HostID: hostID, Credentials: credentials, Metadata: Metadata{Name: r.Name, Operation: r.Operation, Mode: r.Action, State: "prepared", Config: *r.Config, CreatedAt: m.timestamp()}}
			if r.Action == "connect" {
				existing.Metadata.RepositoryID = r.ExpectedID
				existing.Metadata.Format = 2
			}
			if err := s.save(existing); err != nil {
				return response, err
			}
		}
	} else {
		if existing == nil {
			return response, errMissing
		}
		if r.Action == "resume" && r.Operation != existing.Metadata.Operation {
			return response, errConflict
		}
		if r.Action == "verify" && existing.Metadata.RepositoryID == "" {
			return response, errResume
		}
	}
	response.Repository = &existing.Metadata
	if m.Backend == nil {
		return response, errVerify
	}
	if existing.Metadata.Mode == "create" && !existing.InitAttempted {
		if r.Action == "verify" {
			return response, errResume
		}
		if err := m.Backend.CheckEmpty(ctx, existing.Metadata.Config, existing.Credentials); err != nil {
			return response, errPreflight
		}
		existing.InitAttempted = true
		existing.Metadata.State = "initializing"
		if err := s.save(existing); err != nil {
			return response, err
		}
		// This is the sole initialization call. Even a known tool failure can have
		// written remote key/config objects; subsequent requests observe, never retry.
		identity, err := m.Backend.Init(ctx, existing.Metadata.Config, existing.Credentials)
		if err != nil || backupbackend.ValidateIdentity(identity) != nil {
			return response, errUnknown
		}
		existing.Metadata.RepositoryID = identity.ID
		existing.Metadata.Format = identity.Version
		if err = s.save(existing); err != nil {
			return response, err
		}
	}
	identity, err := m.Backend.Probe(ctx, existing.Metadata.Config, existing.Credentials)
	if err != nil || backupbackend.ValidateIdentity(identity) != nil || (existing.Metadata.RepositoryID != "" && identity.ID != existing.Metadata.RepositoryID) {
		if existing.Metadata.RepositoryID != "" {
			existing.Metadata.State = "unverified"
			if saveErr := s.save(existing); saveErr != nil {
				return response, saveErr
			}
		}
		if existing.Metadata.Mode == "create" && existing.Metadata.RepositoryID == "" {
			return response, errUnknown
		}
		return response, errVerify
	}
	existing.Metadata.RepositoryID = identity.ID
	existing.Metadata.Format = identity.Version
	existing.Metadata.State = "ready"
	existing.Metadata.VerifiedAt = m.timestamp()
	if err := s.save(existing); err != nil {
		return response, err
	}
	return response, nil
}
