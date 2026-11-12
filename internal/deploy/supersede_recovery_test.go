package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestSupersedeCleanupRecovery(t *testing.T) {
	for _, interrupted := range []string{"update", "stop", "rm"} {
		t.Run(interrupted, func(t *testing.T) {
			f, r := flowSetup(t)
			current := f.execute(r, "live", false).Current
			route := f.live
			r.Operation = strings.Repeat("e", 32)
			f.unhealthy = true
			failed := f.execute(r, "failed", true)
			if failed.Phase != "starting" || failed.Candidate == nil {
				t.Fatal(failed)
			}
			name := failed.Candidate.Container
			q := r
			q.Operation = strings.Repeat("f", 32)
			q.Supersede = r.Operation
			injected := false
			f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
				result, err := f.run(c, n, a, b)
				if n == "docker" && a[0] == interrupted && a[len(a)-1] == name && !injected {
					injected = true
					return result, errors.New("cleanup response lost")
				}
				return result, err
			}
			j := f.execute(q, "replacement", true)
			if !injected || j.Operation != r.Operation || j.Phase != "starting" {
				t.Fatal("cleanup interruption not retained", j)
			}
			f.unchanged(current, route)
			if interrupted != "rm" {
				if _, err := f.m.candidate(context.Background(), j, j.Candidate); err == nil {
					t.Fatal("normal adoption accepted retiring candidate")
				}
			} else if f.containers[name] != nil {
				t.Fatal("rm side effect missing")
			}
			// Fail the new build so cleanup can be checked without replacing the live release.
			f.buildFail = true
			j = f.execute(q, "replacement", true)
			if j.Operation != q.Operation || j.Phase != "building" || j.Candidate != nil || f.containers[name] != nil {
				t.Fatal("cleanup retry did not complete", j)
			}
			f.unchanged(current, route)
		})
	}
}
