package ssh

import (
	"encoding/json"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/secrets"
	"testing"
)

func TestBatchSecretArchiveAndAcknowledgement(t *testing.T) {
	r := deploy.Request{Action: "secrets-set", Operation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SecretNames: []string{"a", "b"}}
	metadata, _ := json.Marshal(r)
	for _, input := range []string{`{"a":"private-canary","b":"value"}`, `{"a":"x","a":"y","b":"z"}`, `{"a":"x"}`} {
		_, err := actionArchive(metadata, []byte("binary"), []byte(input), r.Action)
		if (err == nil) != (input == `{"a":"private-canary","b":"value"}`) {
			t.Fatalf("unexpected archive validation: %v", err)
		}
	}
	resp := deploy.Response{SetApplied: true, SecretVersions: []secrets.Entry{{Name: "a", Version: secrets.VersionFor(r.Operation, "a")}, {Name: "b", Version: secrets.VersionFor(r.Operation, "b")}}}
	if !validSecretResponse(r, resp) {
		t.Fatal("valid batch rejected")
	}
	resp.SetApplied = false
	if validSecretResponse(r, resp) {
		t.Fatal("missing application acknowledgement accepted")
	}
	resp.SetApplied = true
	resp.SecretVersions[1].Version = r.Operation
	if validSecretResponse(r, resp) {
		t.Fatal("mismatched version accepted")
	}
	resp.SecretVersions = nil
	if validSecretResponse(r, resp) {
		t.Fatal("missing metadata accepted")
	}
}
