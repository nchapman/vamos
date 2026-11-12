package executor

import (
	"bytes"
	"encoding/json"
	"github.com/nchapman/soar/internal/deploy"
	"strings"
	"testing"
)

func TestRunResponseOutputBounded(t *testing.T) {
	for _, status := range []bool{false, true} {
		result := &deploy.RunResult{Output: strings.Repeat("x", 20000) + "tail"}
		response := deploy.Response{Version: 1}
		if status {
			response.Status = &deploy.Status{Run: result}
		} else {
			response.Run = result
		}
		var out bytes.Buffer
		writeAppResponse(&out, response)
		var got deploy.Response
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		r := got.Run
		if status {
			r = got.Status.Run
		}
		if r == nil || len(r.Output) > 8300 || !strings.HasSuffix(r.Output, "tail") {
			t.Fatalf("unbounded/missing output: %s", out.Bytes())
		}
	}
}
