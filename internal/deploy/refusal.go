package deploy

import (
	"errors"
	"github.com/nchapman/soar/internal/secrets"
)

var ErrBindingRequired = errors.New("service binding required")

const CodeBindingRequired = "binding_required"

var (
	errHostBusy     = errors.New("host mutation already active")
	errHostIdentity = errors.New("expected ready host identity does not match")
	errForeignApp   = errors.New("foreign app journal")
)

// ErrorCode classifies identities, never diagnostic strings.
func ErrorCode(err error) string {
	for _, c := range refusalClasses {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

var refusalClasses = []struct {
	err           error
	code, message string
	stageOnly     bool
}{
	{ErrBindingRequired, CodeBindingRequired, "", false},
	{secrets.ErrVersionLimit, "secret_version_limit", "Secret store has reached its 256 retained-version limit; no versions are automatically deleted. Inspect retained versions before planning recovery.", true},
	{secrets.ErrNameLimit, "secret_name_limit", "Secret store has reached its 64-name limit. Inspect existing names before staging another name.", true},
	{secrets.ErrSizeLimit, "secret_size_limit", "Secret store has reached its 1 MiB size limit; no versions are automatically deleted. Inspect retained versions before planning recovery.", true},
	{secrets.ErrOperationConflict, "secret_operation_conflict", "This operation/version already has different content. Inspect retained versions; use a new staging operation for a different value.", true},
	{errHostBusy, "host_busy", "Another host mutation is active. Wait for it to finish before retrying.", true},
	{errHostIdentity, "host_identity_mismatch", "The ready host does not match the expected identity. Verify the selected server and its status before retrying.", false},
	{errForeignApp, "foreign_app", "The app journal belongs to a different host identity. Verify the selected server; manual recovery is required.", false},
	{secrets.ErrInvalidStore, "secret_store_invalid", "Secret store content or identity is invalid. Verify the selected server; manual recovery is required before retrying.", false},
	{secrets.ErrUnsafeStore, "secret_store_unsafe", "Secret store ownership, permissions, or file type is unsafe. Have the host administrator repair the private store before retrying.", false},
}

// RefusalMessage returns only local, action-appropriate explanatory text.
func RefusalMessage(action, code string) string {
	if action != "secrets-stage" && action != "secrets-list" {
		return ""
	}
	for _, c := range refusalClasses {
		if c.code == code && (!c.stageOnly || action == "secrets-stage") {
			return c.message
		}
	}
	return ""
}
