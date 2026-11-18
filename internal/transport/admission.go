package transport

import (
	"errors"
	"fmt"
)

// ErrNotSubmitted marks a failure before an app request could be admitted.
// Never infer it from an SSH exit code or a missing acknowledgement.
var ErrNotSubmitted = errors.New("request not submitted")

// ErrOperationFailed means the coordinator recorded a terminal failure, not
// necessarily that the deployment itself captured resumable inputs.
var ErrOperationFailed = errors.New("coordinator recorded a failed operation")

func NotSubmitted(cause error) error { return fmt.Errorf("%w: %w", ErrNotSubmitted, cause) }
