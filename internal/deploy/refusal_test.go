package deploy

import (
	"errors"
	"fmt"
	"github.com/nchapman/vamos/internal/secrets"
	"testing"
)

func TestSecretRefusalClassification(t *testing.T) {
	for _, err := range []error{secrets.ErrVersionLimit, secrets.ErrNameLimit, secrets.ErrSizeLimit, secrets.ErrOperationConflict} {
		code := ErrorCode(fmt.Errorf("canary: %w", err))
		if code == "" || RefusalMessage("secrets-stage", code) == "" {
			t.Fatalf("unclassified %v", err)
		}
		if RefusalMessage("secrets-list", code) != "" {
			t.Fatal("stage code accepted for list")
		}
	}
	if ErrorCode(errors.New(secrets.ErrVersionLimit.Error())) != "" {
		t.Fatal("string classified")
	}
	if RefusalMessage("secrets-stage", "unknown") != "" {
		t.Fatal("unknown accepted")
	}
	if ErrorCode(fmt.Errorf("wrapped: %w", ErrBindingRequired)) != CodeBindingRequired {
		t.Fatal("binding classification")
	}
}
