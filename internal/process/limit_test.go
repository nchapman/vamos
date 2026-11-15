package process_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestExplicitOutputAllowanceRemainsBounded(t *testing.T) {
	for _, mode := range []string{"stdout-flood", "stderr-flood"} {
		r, err := process.RunLimited(context.Background(), os.Args[0], helperArgs(mode), nil, 8*process.OutputLimit)
		if err != nil || len(r.Stdout)+len(r.Stderr) != 4*process.OutputLimit {
			t.Fatal("larger allowance failed", err)
		}
		r, err = process.RunLimited(context.Background(), os.Args[0], helperArgs(mode), nil, 2*process.OutputLimit)
		if !errors.Is(err, process.ErrOutputLimit) || len(r.Stdout)+len(r.Stderr) != 2*process.OutputLimit {
			t.Fatal("output escaped bound", err)
		}
	}
	for _, limit := range []int{0, -1, (8 << 20) + 1} {
		if _, err := process.RunLimited(context.Background(), "must-not-execute", nil, nil, limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}
