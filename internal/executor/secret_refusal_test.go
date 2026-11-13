package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/secrets"
	"testing"
)

func TestSecretFailureCode(t *testing.T) {
	var b bytes.Buffer
	if appFailure(&b, fmt.Errorf("canary: %w", secrets.ErrOperationConflict), 1) != 1 {
		t.Fatal("exit")
	}
	var r deploy.Response
	if err := json.Unmarshal(b.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Version != 1 || r.ErrorCode != deploy.ErrorCode(secrets.ErrOperationConflict) {
		t.Fatalf("%+v", r)
	}
}
