package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
)

type endlessContext struct{ read int }

func (r *endlessContext) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read += len(p)
	return len(p), nil
}

func TestAppContextReadIsBounded(t *testing.T) {
	r := &endlessContext{}
	if _, err := readAppContext(r); err == nil {
		t.Fatal("accepted oversized streamed context")
	}
	if r.read != (32<<20)+1 {
		t.Fatalf("read %d bytes", r.read)
	}
}

func TestAppRequestStrictDecode(t *testing.T) {
	for _, data := range []string{"", `{"extra":true}`, `{} {}`, strings.Repeat("x", maxAppRequest+1)} {
		if _, err := readAppRequest(strings.NewReader(data)); err == nil {
			t.Errorf("accepted invalid request %q", data[:min(len(data), 30)])
		}
	}
}
func TestAppAuthorizationBeforeFilesystemMutation(t *testing.T) {
	for _, data := range []string{`{"version":2}`, `{"version":1,"action":"destroy"}`, `{"version":1,"action":"deploy","app":"../escape"}`, `{"version":1,"action":"status","app":"hello","host":{"name":"home","domain":"example.com"},"operation":"not-allowed"}`} {
		var out bytes.Buffer
		if code := ServeApp(context.Background(), strings.NewReader(data), &out); code != 2 {
			t.Fatalf("code=%d out=%s", code, &out)
		}
		var response deploy.Response
		if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Error == "" {
			t.Fatal("missing protocol error", err)
		}
	}
}
func TestAppReadCancellationClosesInput(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAppRequestContext(ctx, r); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestAppProtocolBoundsErrorDiagnostics(t *testing.T) {
	var out bytes.Buffer
	appFailure(&out, hugeAppError{}, 1)
	if out.Len() > 32*1024 {
		t.Fatal("error envelope can overflow transport")
	}
}

type hugeAppError struct{}

func (hugeAppError) Error() string { return strings.Repeat("\x00", 100*1024) }
