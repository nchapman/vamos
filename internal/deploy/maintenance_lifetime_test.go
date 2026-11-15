package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

// Exercise the actual inventory planner, not just a fabricated public Plan.
func TestMaintenanceLifetimeReceiptInventory(t *testing.T) {
	for _, count := range []int{500, 1024, 4096} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("receipts=%d/shared=%v", count, shared), func(t *testing.T) {
				f, req := flowSetup(t)
				j := f.execute(req, "archive", false)
				original := f.m.Run
				id := "sha256:" + strings.Repeat("9", 64)
				f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
					if n == "docker" && len(a) > 1 {
						if a[1] == "ls" {
							if a[0] == "image" && shared {
								return process.Result{Stdout: []byte(id + "\n")}, nil
							}
							return process.Result{}, nil
						}
						if a[0] == "image" && a[1] == "inspect" && a[2] == id {
							v := resourceInspection{RepoTags: []string{"foreign:retained"}}
							v.ID = id
							data, _ := json.Marshal([]resourceInspection{v})
							return process.Result{Stdout: data}, nil
						}
					}
					return original(c, n, a, b)
				}
				for i := 0; i < count; i++ {
					name := fmt.Sprintf("%032x.%032x.image.iid", i+1, i+1)
					if err := os.WriteFile(filepath.Join(f.m.dir(j.App), name), []byte(id+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				for i := 0; i < 2; i++ {
					q := req
					q.Config = nil
					q.Action = "cleanup"
					q.Apply = true
					q.Operation = fmt.Sprintf("%032x", 100000+i)
					if _, err := f.m.Execute(context.Background(), q, nil); err != nil {
						t.Fatal(err)
					}
				}
				entries, err := os.ReadDir(f.m.dir(j.App))
				if err != nil {
					t.Fatal(err)
				}
				receipts := 0
				for _, e := range entries {
					if strings.HasSuffix(e.Name(), ".image.iid") {
						receipts++
					}
				}
				if receipts < count {
					t.Fatal("cleanup discarded immutable receipts")
				}
				q := req
				q.Config = nil
				q.Action = "destroy"
				q.Operation = strings.Repeat("e", 32)
				resp, err := f.m.Execute(context.Background(), q, nil)
				if err != nil {
					t.Fatalf("large receipt history blocks actual planner: %v", err)
				}
				data, err := json.Marshal(resp)
				if err != nil || len(data) > 64<<10 {
					t.Fatalf("response size %d: %v", len(data), err)
				}
				q.Apply = true
				// Force a stop after durable intent installation but before resource work.
				f.reloadCrash = "before"
				if _, err = f.m.Execute(context.Background(), q, nil); err == nil {
					t.Fatal("expected interrupted route withdrawal")
				}
				j, err = f.m.load(j.App)
				if err != nil {
					t.Fatal(err)
				}
				if j.Maintenance == nil {
					t.Fatal("missing durable intent")
				}
				f.reloadCrash = ""
				f.adminFail = false
				q.Action = "resume"
				q.Apply = false
				resp, err = f.m.Execute(context.Background(), q, nil)
				if err != nil {
					t.Fatal(err)
				}
				if resp.Status.Phase != "destroyed" {
					t.Fatalf("not destroyed: %+v", resp.Status)
				}
				entries, err = os.ReadDir(f.m.dir(j.App))
				if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
					t.Fatalf("not minimal: %v %v", entries, err)
				}
			})
		}
	}
}
