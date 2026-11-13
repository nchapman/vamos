package executor

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
)

func TestRollbackPreviewResponseRoundTrip(t *testing.T) {
	preview := &deploy.RollbackPreview{TargetOperation: "previous", NeedsConfirmation: true, Services: []deploy.RollbackService{{Name: "web", CurrentImage: "new", TargetImage: "old", Downtime: true, SecretVersions: map[string]string{"TOKEN": "captured-version"}}, {Name: "worker", ExcludedReason: "no previous image"}}}
	var out bytes.Buffer
	if code := writeAppResponse(&out, deploy.Response{Version: 1, RollbackPreview: preview}); code != 0 {
		t.Fatalf("code=%d: %s", code, &out)
	}
	var got deploy.Response
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RollbackPreview, preview) {
		t.Fatalf("preview changed: %+v", got.RollbackPreview)
	}
}
