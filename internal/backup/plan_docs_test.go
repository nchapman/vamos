package backup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

// Test the actual documentation, not copied examples that can drift independently.
func TestDocumentedBackupPlans(t *testing.T) {
	for _, name := range []string{"backup-plans.md", "backups.md"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
			if err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(strings.NewReader(string(data)))
			var text strings.Builder
			inPlan := false
			examples := 0
			apps := map[string]bool{}
			for scanner.Scan() {
				line := scanner.Text()
				if !inPlan {
					if line == "```yaml" {
						inPlan = true
						text.Reset()
					}
					continue
				}
				if line != "```" {
					text.WriteString(line)
					text.WriteByte('\n')
					continue
				}
				inPlan = false
				examples++
				t.Run(fmt.Sprintf("example-%d", examples), func(t *testing.T) {
					p, err := DecodePlan(strings.NewReader(text.String()))
					if err != nil {
						t.Fatal(err)
					}
					apps[p.App] = true
					if p.App != "journal" && p.App != "rails-ledger" {
						t.Fatalf("add a corresponding app fixture for %s", p.App)
					}
					app, err := config.Load(filepath.Join("..", "..", "examples", p.App, "soar.yaml"))
					if err != nil {
						t.Fatal(err)
					}
					if app.Name != p.App {
						t.Fatal("plan/app name mismatch")
					}
					for sourceName, s := range p.Sources {
						svc, ok := app.Services[s.Service]
						if !ok {
							t.Fatalf("%s: unknown service", sourceName)
						}
						if s.Type == "postgres" {
							credentials, ok := app.Services[s.Credentials.Service]
							if !ok || credentials.Secrets[s.Credentials.Env] == "" {
								t.Fatalf("%s: selector is not secret-backed", sourceName)
							}
							if s.Database != svc.Env["POSTGRES_DB"] || s.User != svc.Env["POSTGRES_USER"] || svc.ReplicaCount() != 1 {
								t.Fatalf("%s: PostgreSQL selection does not match example", sourceName)
							}
						} else if svc.Volume == nil || svc.Volume.Name != s.Volume {
							t.Fatalf("%s: volume does not match example", sourceName)
						}
					}
					for _, set := range p.Sets {
						for _, svc := range set.Stop {
							if _, ok := app.Services[svc]; !ok {
								t.Fatalf("unknown stop service: %s", svc)
							}
						}
					}
				})
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if inPlan || examples < 2 || !apps["journal"] || !apps["rails-ledger"] {
				t.Fatal("expected complete Journal and Rails plan examples")
			}
		})
	}
}
