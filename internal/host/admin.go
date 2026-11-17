package host

import (
	"errors"
	"fmt"
	"strings"
)

// ErrAdminPrerequisite identifies local, secret-free host admission diagnostics.
var ErrAdminPrerequisite = errors.New("host prerequisites not met")

// RequireAdmin explains prerequisite failures without conflating host support
// with privileges. Administrative SSH inspection may elevate using sudo -n.
func RequireAdmin(r *Report) error {
	if r == nil {
		return fmt.Errorf("%w: host inspection returned no report; run vamos server inspect TARGET", ErrAdminPrerequisite)
	}
	if !r.Supported {
		reason := strings.Join(r.Problems, "; ")
		if reason == "" {
			reason = "Ubuntu 24.04+ with systemd and amd64/arm64 is required"
		}
		return fmt.Errorf("%w: unsupported host: %s", ErrAdminPrerequisite, reason)
	}
	if !r.Root {
		return fmt.Errorf("%w: administration requires root or passwordless sudo (inspected UID %d); verify sudo -n true using the same SSH account", ErrAdminPrerequisite, r.UID)
	}
	return nil
}
