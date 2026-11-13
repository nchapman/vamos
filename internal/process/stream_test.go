package process_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/process"
)

func TestStreamPreservesLargeBinaryInputAndSeparateOutput(t *testing.T) {
	input := bytes.Repeat([]byte("quotes ' \" ; $()\n\x00\xffcafé"), 10000)
	var stdout, stderr bytes.Buffer
	code, err := process.Stream(context.Background(), os.Args[0], helperArgs("echo"), bytes.NewReader(input), &stdout, &stderr)
	if err != nil || code != 0 || !bytes.Equal(stdout.Bytes(), input) || stderr.String() != "diagnostic" {
		t.Fatalf("code=%d stdout=%d stderr=%q err=%v", code, stdout.Len(), stderr.String(), err)
	}
}

func TestStreamExitAndCancellation(t *testing.T) {
	t.Run("known nonzero exit", func(t *testing.T) {
		var stderr bytes.Buffer
		code, err := process.Stream(context.Background(), os.Args[0], helperArgs("fail"), nil, io.Discard, &stderr)
		if err != nil || code != 23 || stderr.String() != "failure" {
			t.Fatalf("code=%d stderr=%q err=%v", code, stderr.String(), err)
		}
	})
	t.Run("no stdin", func(t *testing.T) {
		var stdout bytes.Buffer
		code, err := process.Stream(context.Background(), os.Args[0], helperArgs("echo"), nil, &stdout, io.Discard)
		if err != nil || code != 0 || stdout.Len() != 0 {
			t.Fatalf("code=%d stdout=%q err=%v", code, stdout.String(), err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		code, err := process.Stream(ctx, os.Args[0], helperArgs("hang"), nil, io.Discard, io.Discard)
		if code != -1 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		code, err := process.Stream(context.Background(), "soar-test-missing-executable", nil, nil, io.Discard, io.Discard)
		if code != -1 || err == nil {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	t.Run("inherited pipes", func(t *testing.T) {
		code, err := process.Stream(context.Background(), os.Args[0], helperArgs("inherit"), nil, io.Discard, io.Discard)
		if code != -1 || !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
}
