package executor

import (
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/secrets"
	"os"
	"path/filepath"
	"testing"
)

func TestReadBatchSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.value")
	r := deploy.Request{Action: "secrets-set", SecretNames: []string{"a", "b"}}
	for _, data := range []string{`{"a":"x","b":"y"}`, `{"a":"x","a":"z","b":"y"}`} {
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readSecretsFile(p, r)
		if (err == nil) != (data == `{"a":"x","b":"y"}`) {
			t.Fatalf("validation: %v", err)
		}
	}
	if err := os.Truncate(p, secrets.MaxBatchBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretsFile(p, r); err == nil {
		t.Fatal("oversized batch accepted")
	}
}
