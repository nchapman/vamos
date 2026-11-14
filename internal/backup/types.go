// Package backup owns server-level repository setup. It does not capture or
// restore application data; repository verification is not a recovery drill.
package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/nchapman/soar/internal/backupbackend"
)

const MaxPrivateBytes = 64 * 1024
const MaxRepositories = 32

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var operationPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidName(name string) bool    { return namePattern.MatchString(name) }
func ValidOperation(id string) bool { return operationPattern.MatchString(id) }

type Request struct {
	Action     string                `json:"action"`
	Name       string                `json:"name,omitempty"`
	Operation  string                `json:"operation,omitempty"`
	Config     *backupbackend.Config `json:"config,omitempty"`
	ExpectedID string                `json:"expected_id,omitempty"`
	Confirm    bool                  `json:"confirm,omitempty"`
}

// Metadata deliberately excludes credentials and raw backend diagnostics.
type Metadata struct {
	Name         string               `json:"name"`
	Operation    string               `json:"operation,omitempty"`
	Mode         string               `json:"mode"`
	State        string               `json:"state"`
	Config       backupbackend.Config `json:"config"`
	RepositoryID string               `json:"repository_id,omitempty"`
	Format       int                  `json:"format,omitempty"`
	CreatedAt    string               `json:"created_at,omitempty"`
	VerifiedAt   string               `json:"verified_at,omitempty"`
}

type Response struct {
	Repository   *Metadata  `json:"repository,omitempty"`
	Repositories []Metadata `json:"repositories"`
}

func (r Request) Mutating() bool { return r.Action != "list" && r.Action != "status" }
func (r Request) NeedsCredentials() bool {
	return r.Action == "create" || r.Action == "connect" || r.Action == "probe"
}

func ValidateRequest(r Request) error {
	if r.Action == "list" {
		if r.Name != "" || r.Operation != "" || r.Config != nil || r.ExpectedID != "" || r.Confirm {
			return errors.New("repository list accepts no selection or mutation fields")
		}
		return nil
	}
	if !ValidName(r.Name) {
		return errors.New("invalid repository name")
	}
	switch r.Action {
	case "create", "connect", "probe":
		if r.Config == nil {
			return errors.New("repository configuration is required")
		}
		if err := backupbackend.ValidateConfig(*r.Config); err != nil {
			return errors.New("invalid repository configuration")
		}
		if r.Action == "probe" {
			if r.Operation != "" || r.Confirm || r.ExpectedID != "" {
				return errors.New("repository probe accepts configuration only")
			}
			return nil
		}
		if !r.Confirm || !ValidOperation(r.Operation) {
			return errors.New("repository setup requires confirmation and operation identity")
		}
		if r.Action == "create" {
			if r.ExpectedID != "" || !strings.HasSuffix(r.Config.Prefix, "/"+r.Operation) {
				return errors.New("new repositories require a unique operation-scoped prefix")
			}
		} else if backupbackend.ValidateIdentity(backupbackend.Identity{ID: r.ExpectedID, Version: 2}) != nil {
			return errors.New("connecting requires the full expected repository ID")
		}
	case "resume":
		if !ValidOperation(r.Operation) || r.Config != nil || r.ExpectedID != "" || r.Confirm {
			return errors.New("resume requires only repository name and original operation identity")
		}
	case "status", "verify":
		if r.Operation != "" || r.Config != nil || r.ExpectedID != "" || r.Confirm {
			return errors.New("repository observation accepts only a name")
		}
	default:
		return errors.New("unsupported repository action")
	}
	return nil
}

func DecodeCredentials(data []byte) (backupbackend.Credentials, error) {
	var c backupbackend.Credentials
	if len(data) == 0 || len(data) > MaxPrivateBytes {
		return c, errors.New("invalid private repository input")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return backupbackend.Credentials{}, errors.New("invalid private repository input")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return backupbackend.Credentials{}, errors.New("invalid private repository input")
	}
	if err := backupbackend.ValidateCredentials(c); err != nil {
		return backupbackend.Credentials{}, errors.New("invalid private repository credentials")
	}
	return c, nil
}

// RecoveryKit is private operator material, never part of the metadata protocol.
// The pending kit is written before setup so a lost response cannot lose its key.
type RecoveryKit struct {
	Version       int                       `json:"version"`
	Name          string                    `json:"name"`
	Operation     string                    `json:"operation"`
	Config        backupbackend.Config      `json:"config"`
	Credentials   backupbackend.Credentials `json:"credentials"`
	RepositoryID  string                    `json:"repository_id,omitempty"`
	Format        int                       `json:"format"`
	ResticVersion string                    `json:"restic_version"`
	Notice        string                    `json:"notice"`
}
