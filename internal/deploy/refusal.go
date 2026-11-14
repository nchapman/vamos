package deploy

import (
	"errors"
	"github.com/nchapman/soar/internal/backup"
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
	if errors.Is(err, errBackupPlanLegacy) {
		return "backup_plan_legacy"
	}
	if errors.Is(err, errBackupPlanAppState) {
		return "backup_plan_app_state"
	}
	if errors.Is(err, errBackupPlanRuntime) {
		return "backup_plan_runtime"
	}
	if code := backup.ErrorCode(err); code != "" {
		return code
	}
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
	{errSecretSetIncomplete, "secret_set_incomplete", "Secret application requires a completed deployment. Inspect status and finish recovery before setting secrets; no new values were staged.", true},
	{errSecretSetProtected, "secret_set_protected", "A consumer is protected by update: manual. No new values were staged. Stage the secrets instead, then explicitly deploy the protected service with --service NAME --yes.", true},
	{errSecretSetLegacy, "secret_set_legacy", "Secret application requires captured multi-service configuration. No new values were staged; deploy with a current client first.", true},
	{errSecretSetUnavailable, "secret_set_unavailable", "A consumer or captured image is unavailable or has drifted. Inspect status before recovery.", true},
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
	if action == "backup-plan" && code == "backup_plan_legacy" {
		return errBackupPlanLegacy.Error()
	}
	if action == "backup-plan" && code == "backup_plan_app_state" {
		return errBackupPlanAppState.Error()
	}
	if action == "backup-plan" && code == "backup_plan_runtime" {
		return errBackupPlanRuntime.Error()
	}
	if action == "backup-repository" || action == "backup-plan" {
		if message := backup.RefusalMessage(code); message != "" {
			return message
		}
		if code == "host_busy" || code == "host_identity_mismatch" {
			for _, c := range refusalClasses {
				if c.code == code {
					return c.message
				}
			}
		}
		return ""
	}
	if action != "secrets-stage" && action != "secrets-set" && action != "secrets-list" {
		return ""
	}
	for _, c := range refusalClasses {
		if c.code == code && (!c.stageOnly || action == "secrets-stage" || action == "secrets-set") {
			return c.message
		}
	}
	return ""
}
