package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestHistoricalStatelessVolumeFieldsRemainReadable(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "v2"
		if legacy {
			name = "v1"
		}
		t.Run(name, func(t *testing.T) {
			f := newMultiFlow(t)
			q := f.req
			q.Operation = strings.Repeat("1", 32)
			data := f.bundle(q.Config)
			if legacy {
				q.Config = app()
				q.ContextFormat = ""
				data = []byte("captured:Dockerfile")
			}
			if _, err := f.m.Execute(context.Background(), q, data); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(f.m.dir(q.App), "state.json")
			original, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			for _, empty := range []any{nil, []any{}} {
				var document any
				if err := json.Unmarshal(original, &document); err != nil {
					t.Fatal(err)
				}
				var addHistoricalFields func(any)
				addHistoricalFields = func(value any) {
					switch node := value.(type) {
					case map[string]any:
						if _, service := node["Build"]; service {
							node["Volumes"] = empty
						}
						for _, child := range node {
							addHistoricalFields(child)
						}
					case []any:
						for _, child := range node {
							addHistoricalFields(child)
						}
					}
				}
				addHistoricalFields(document)
				historical, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, historical, 0600); err != nil {
					t.Fatal(err)
				}
				r := q
				r.Action = "status"
				r.Operation = ""
				r.ContextFormat = ""
				r.Config = nil
				response, err := f.m.Execute(context.Background(), r, nil)
				if err != nil || response.Status == nil || response.Status.Phase != "ready" || !response.Status.Running {
					t.Fatalf("historical journal became unreadable: %+v %v", response, err)
				}
				j := f.load()
				if err := f.m.save(j); err != nil {
					t.Fatal(err)
				}
				canonical, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(canonical), `"Volumes"`) {
					t.Fatal("historical field re-emitted")
				}
			}
			// The compatibility decode must also preserve effective configuration well
			// enough for a current client to update/migrate the deployment normally.
			a := changedMultiApp("updated")
			if legacy {
				delete(a.Services, "backend")
			}
			if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSingularVolumeRequiresCurrentDeploymentProtocol(t *testing.T) {
	for _, public := range []bool{true, false} {
		a := app()
		s := a.Services["web"]
		s.Volume = &config.Volume{Name: "data", Mount: "/data"}
		if !public {
			s.HTTP = nil
		}
		a.Services["web"] = s
		if err := a.Validate(); err != nil {
			t.Fatal("valid schema rejected:", err)
		}
		if err := ValidateApp(a); err != nil {
			t.Fatal(err)
		}
		_, request := setup(t)
		request.Config = a
		if err := ValidateRequest(request); err == nil {
			t.Fatal("legacy volume deployment accepted")
		}
		request.ContextFormat = BundleFormat
		if err := ValidateRequest(request); err != nil {
			t.Fatal(err)
		}
	}
}
