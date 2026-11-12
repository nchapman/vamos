package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/host"
)

const validRequest = `{"version":1,"operation":"inspect"}`
const goodProbe = "soar-inspect-v1\nLinux\nx86_64\n0\nsystemd\nyes\npresent\nID=ubuntu\nVERSION_ID=24.04\n"

func TestProtocolInspection(t *testing.T) {
	for _, supported := range []bool{true, false} {
		probe := goodProbe
		if !supported {
			probe = strings.Replace(probe, "24.04", "22.04", 1)
		}
		var stdout bytes.Buffer
		code := serve(context.Background(), strings.NewReader(validRequest), &stdout, func(ctx context.Context) ([]byte, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("probe has no deadline")
			}
			return []byte(probe), nil
		})
		response := readResponse(t, &stdout)
		if code != 0 || response.Version != 1 || response.Error != nil || response.Result == nil || response.Result.Supported != supported {
			t.Fatalf("code=%d response=%+v", code, response)
		}
		expected, _ := host.Parse([]byte(probe))
		want, _ := json.Marshal(expected)
		got, _ := json.Marshal(response.Result)
		if !bytes.Equal(want, got) {
			t.Fatal("executor and bootstrap parser disagree")
		}
	}
}

func TestInvalidRequestsDoNotProbe(t *testing.T) {
	tests := []struct{ input, code string }{
		{"", "invalid_request"},
		{"{", "invalid_request"},
		{`{"version":1,"operation":"inspect","command":"touch /tmp/no"}`, "invalid_request"},
		{validRequest + "\n{}", "invalid_request"},
		{validRequest + strings.Repeat(" ", maxRequest), "invalid_request"},
		{`{"version":2,"operation":"inspect"}`, "unsupported_version"},
		{`{"version":1,"operation":"deploy"}`, "unsupported_operation"},
	}
	for _, tt := range tests {
		t.Run(tt.code+"/"+tt.input[:min(30, len(tt.input))], func(t *testing.T) {
			var stdout bytes.Buffer
			code := serve(context.Background(), strings.NewReader(tt.input), &stdout, func(context.Context) ([]byte, error) {
				t.Fatal("invalid request executed a probe")
				return nil, nil
			})
			r := readResponse(t, &stdout)
			if code != 2 || r.Result != nil || r.Error == nil || r.Error.Code != tt.code {
				t.Fatalf("code=%d response=%+v", code, r)
			}
		})
	}
}

func TestProbeFailureIsJSON(t *testing.T) {
	var stdout bytes.Buffer
	code := serve(context.Background(), strings.NewReader(validRequest), &stdout, func(context.Context) ([]byte, error) {
		return nil, errors.New("probe failed")
	})
	r := readResponse(t, &stdout)
	if code != 1 || r.Error == nil || r.Error.Code != "inspection_failed" || r.Result != nil {
		t.Fatalf("code=%d response=%+v", code, r)
	}
}

func TestCancelledRequestReadDoesNotHang(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	type result struct {
		code   int
		output string
	}
	done := make(chan result, 1)
	go func() {
		var stdout bytes.Buffer
		code := serve(ctx, reader, &stdout, func(context.Context) ([]byte, error) {
			return nil, errors.New("must not probe before request EOF")
		})
		done <- result{code, stdout.String()}
	}()
	select {
	case got := <-done:
		if got.code != 2 || !strings.Contains(got.output, "deadline exceeded") {
			t.Fatalf("result=%+v", got)
		}
	case <-time.After(500 * time.Millisecond):
		writer.Close()
		<-done
		t.Fatal("request read ignored context cancellation")
	}
}

func TestServeLocal(t *testing.T) {
	var stdout bytes.Buffer
	code := Serve(context.Background(), strings.NewReader(validRequest), &stdout)
	r := readResponse(t, &stdout)
	if runtime.GOOS == "linux" {
		if code != 0 || r.Result == nil || r.Result.Kernel != "Linux" {
			t.Fatalf("native executor: code=%d response=%+v", code, r)
		}
	} else if code != 1 || r.Error == nil || r.Error.Code != "inspection_failed" {
		t.Fatalf("non-Linux executor: code=%d response=%+v", code, r)
	}
}

func TestResponseWriteFailure(t *testing.T) {
	code := serve(context.Background(), strings.NewReader(validRequest), failingWriter{}, func(context.Context) ([]byte, error) {
		return []byte(goodProbe), nil
	})
	if code != 1 {
		t.Fatalf("got %d", code)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func readResponse(t *testing.T, reader io.Reader) Response {
	t.Helper()
	var response Response
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra output after response: %v", err)
	}
	return response
}
