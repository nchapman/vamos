package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installedPlan(t *testing.T) (*Manager, string, PlanRecord, string) {
	t.Helper()
	m, h, p, rt := planFixture(t)
	pre := previewPlan(t, m, h, p, rt)
	r, e := m.ExecutePlan(context.Background(), h, p.App, approval(&p, pre), rt)
	if e != nil {
		t.Fatal(e)
	}
	return m, h, *r.Current, filepath.Join(m.planDirectory().dir, p.App+".json")
}
func TestPlanStorePrivateAndCAS(t *testing.T) {
	m, h, r, file := installedPlan(t)
	for _, p := range []string{file, m.planDirectory().dir} {
		i, e := os.Stat(p)
		if e != nil {
			t.Fatal(e)
		}
		if i.Mode().Perm()&0077 != 0 {
			t.Fatalf("not private: %s %v", p, i.Mode())
		}
	}
	before, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.savePlan(r, "none"); !errors.Is(e, errPlanConflict) {
		t.Fatalf("CAS: %v", e)
	}
	after, e := os.ReadFile(file)
	if e != nil || string(before) != string(after) {
		t.Fatal("CAS changed record", e)
	}
	if _, e = m.loadPlan(r.Plan.App, h); e != nil {
		t.Fatal(e)
	}
}
func TestPlanStoreFailsClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "foreign", "mode", "symlink", "trailing", "unknown-field", "unknown-schema", "hash", "parent-writable"} {
		t.Run(kind, func(t *testing.T) {
			m, h, r, file := installedPlan(t)
			b, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "corrupt":
				b = []byte("{")
			case "foreign":
				h = strings.Repeat("f", 32)
			case "mode":
				if e = os.Chmod(file, 0644); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				target := file + ".target"
				if e = os.Rename(file, target); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(target, file); e != nil {
					t.Fatal(e)
				}
			case "trailing":
				b = append(b, []byte(" {}")...)
			case "unknown-field":
				b = append([]byte(`{"unknown":true,`), b[1:]...)
			case "unknown-schema":
				r.Version = 2
				r.Revision, _ = PlanRevision(r)
				b, e = json.Marshal(r)
			case "hash":
				r.Bindings.Stops["web"] = strings.Repeat("9", 32)
				b, e = json.Marshal(r)
			case "parent-writable":
				if e = os.Chmod(filepath.Join(m.Root, "var/lib/vamos"), 0777); e != nil {
					t.Fatal(e)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if kind != "mode" && kind != "symlink" && kind != "parent-writable" {
				if e = os.WriteFile(file, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = m.loadPlan(r.Plan.App, h); !errors.Is(e, errPlanState) {
				t.Fatalf("unsafe record accepted: %v", e)
			}
		})
	}
}
func TestPlanRecordHashCoversPolicyAndBindings(t *testing.T) {
	_, _, original, _ := installedPlan(t)
	for _, kind := range []string{"host", "policy", "volume", "repository"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := json.Marshal(original)
			var r PlanRecord
			if e := json.Unmarshal(b, &r); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "host":
				r.HostID = strings.Repeat("e", 32)
			case "policy":
				s := r.Plan.Sources["db"]
				s.Path = "other.sqlite"
				r.Plan.Sources["db"] = s
			case "volume":
				r.Bindings.Sources["db"].Volume.ID = strings.Repeat("9", 32)
			case "repository":
				pin := r.Bindings.Repositories["offsite"]
				pin.ID = strings.Repeat("9", 64)
				r.Bindings.Repositories["offsite"] = pin
			}
			if ValidatePlanRecord(r) == nil {
				t.Fatal("changed immutable record accepted without new hash")
			}
		})
	}
}
func TestPlanRecordServiceVolumeConsistency(t *testing.T) {
	for _, kind := range []string{"same-service-different-volume", "different-service-duplicate-volume", "different-service-same-incarnation"} {
		t.Run(kind, func(t *testing.T) {
			_, _, r, _ := installedPlan(t)
			b := r.Bindings.Sources["uploads"]
			if kind == "same-service-different-volume" {
				v := *b.Volume
				v.ID = strings.Repeat("9", 32)
				b.Volume = &v
			} else {
				s := r.Plan.Sources["uploads"]
				s.Service = "worker"
				s.Volume = "worker-data"
				r.Plan.Sources["uploads"] = s
				set := r.Plan.Sets["daily"]
				set.Stop = append(set.Stop, "worker")
				r.Plan.Sets["daily"] = set
				b.Service = "worker"
				v := *b.Volume
				v.Name = s.Volume
				if kind == "different-service-duplicate-volume" {
					b.Incarnation = strings.Repeat("9", 32)
				} else {
					v.ID = strings.Repeat("9", 32)
				}
				b.Volume = &v
				r.Bindings.Stops["worker"] = b.Incarnation
			}
			r.Bindings.Sources["uploads"] = b
			var e error
			r.Plan, e = NormalizePlan(r.Plan)
			if e != nil {
				t.Fatal(e)
			}
			r.Revision, e = PlanRevision(r)
			if e != nil {
				t.Fatal(e)
			}
			err := ValidatePlanRecord(r)
			if kind == "different-service-same-incarnation" {
				// Newly created services share the deployment operation token;
				// identity is (service name, incarnation), not incarnation alone.
				if err != nil {
					t.Fatalf("valid service-scoped incarnation rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("inconsistent or duplicate volume identity accepted")
			}

		})
	}
}
