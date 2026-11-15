package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/secrets"
)

func TestAppResponseBudgetPreservesCompleteJSON(t *testing.T) {
	response := deploy.Response{Version: 1, Status: &deploy.Status{}}
	for i := 0; i < 64; i++ {
		s := deploy.SecretStatus{Name: fmt.Sprintf("%061d-%d", i, i%10), StagedVersion: strings.Repeat("a", 32), AppliedVersions: map[string]string{}}
		for k := 0; k < 8; k++ {
			name := fmt.Sprintf("service-%055d", k)
			s.Services = append(s.Services, name)
			s.AppliedVersions[name] = strings.Repeat("b", 32)
		}
		response.Status.Secrets = append(response.Status.Secrets, s)
	}
	var out bytes.Buffer
	if code := writeAppResponse(&out, response); code == 0 {
		t.Fatal("oversized aggregate response accepted")
	}
	var decoded deploy.Response
	if out.Len() > process.OutputLimit || json.Unmarshal(out.Bytes(), &decoded) != nil || !strings.Contains(decoded.Error, "--service") {
		t.Fatal("response was truncated or unactionable")
	}
}

func TestRetainedSecretListFitsResponseBudget(t *testing.T) {
	response := deploy.Response{Version: 1}
	for i := 0; i < secrets.MaxVersions; i++ {
		response.SecretVersions = append(response.SecretVersions, secrets.Entry{Name: strings.Repeat("n", 63), Version: fmt.Sprintf("%032x", i)})
	}
	var out bytes.Buffer
	if code := writeAppResponse(&out, response); code != 0 || out.Len() > process.OutputLimit {
		t.Fatal("supported retained metadata does not fit")
	}
}
