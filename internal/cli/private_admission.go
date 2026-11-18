package cli

import (
	"errors"

	"github.com/nchapman/vamos/internal/agentclient"
	"github.com/nchapman/vamos/internal/transport"
)

// Keep raw SSH and coordinator diagnostics out of credential-bearing commands.
// Access can be revoked between submission and observation: do not claim a
// refused read means that the earlier submission did not happen.
func privateAdmissionMessage(err error) string {
	var remote *agentclient.RemoteError
	forbidden := errors.As(err, &remote) && remote.Code == "forbidden"
	switch {
	case errors.Is(err, transport.ErrNotSubmitted) && forbidden:
		return "Request not submitted: not authorized. Verify the selected target and SSH key grant."
	case errors.Is(err, transport.ErrNotSubmitted):
		return "Request not submitted. Check the selected target, access and server prerequisites."
	case forbidden:
		return "Access denied. Verify the selected target and SSH key grant with the administrator before retrying; earlier work may have been admitted."
	default:
		return ""
	}
}
