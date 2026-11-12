package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPlanRefusesExistingPackagesAndRemovals(t *testing.T) {
	for _, tc := range []struct {
		name, plan string
		baseline   map[string]installedPackage
	}{
		{"upgrade", "Inst libc6 [1] (2 Ubuntu [amd64])\n", map[string]installedPackage{"libc6": {"installed", "1"}}},
		{"adopt-config-files", "Inst caddy (2 Ubuntu [amd64])\n", map[string]installedPackage{"caddy": {"config-files", "1"}}},
		{"remove", "Remv existing [1]\n", nil},
		{"invalid", "Inst ??? (2 Ubuntu [amd64])\n", nil},
		{"qualified-upgrade", "Inst libc6:amd64 [1] (2 Ubuntu [amd64])\n", map[string]installedPackage{"libc6": {"installed", "1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseInstallPlan([]byte(tc.plan), tc.baseline); err == nil {
				t.Fatal("unsafe APT plan accepted")
			}
		})
	}
	packages, err := parseInstallPlan([]byte("Inst caddy (2.6.2-1ubuntu0.24.04.3 Ubuntu:24.04/noble-updates [amd64])\nConf caddy (2.6.2-1ubuntu0.24.04.3 Ubuntu [amd64])\n"), nil)
	if err != nil || len(packages) != 1 || packages[0].Name != "caddy" {
		t.Fatalf("packages=%+v err=%v", packages, err)
	}
}

func TestPurgePlanDoesNotRemoveDependenciesOwnedByOthers(t *testing.T) {
	for _, plan := range []string{"Purg caddy [2]\nRemv other [1]\n", "Inst other (1 Ubuntu [amd64])\n"} {
		if err := checkPurgePlan([]byte(plan), []Package{{"caddy", "2"}}); err == nil {
			t.Fatal("unsafe purge accepted")
		}
	}
	if err := checkPurgePlan([]byte("Purg caddy [2]\n"), []Package{{"caddy", "2"}}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCacheRequiresRecognizedEmptyReport(t *testing.T) {
	for _, data := range []string{`{"Type":"Build Cache","TotalCount":"0"}`, `{"Type":"Build Cache","TotalCount":0}`} {
		if err := checkBuildCache([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range []string{"", `{}`, `{"Type":"Images","TotalCount":"0"}`, `{"Type":"Build Cache","TotalCount":"1"}`, `{"Type":"Build Cache","TotalCount":"unknown"}`} {
		if err := checkBuildCache([]byte(data)); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
}

func TestMountGuardRecognizesDescendantsAndEscapedPaths(t *testing.T) {
	for _, path := range []string{"/var/lib/docker", "/var/lib/docker/overlay2/data", `/var/lib/docker/with\040space`} {
		if err := checkMountInfo([]byte("1 2 0:3 / "+path+" rw - tmpfs tmpfs rw\n"), dataPaths); err == nil {
			t.Errorf("accepted mount %s", path)
		}
	}
	if err := checkMountInfo([]byte("1 2 0:3 / /var/lib/docker-other rw - tmpfs tmpfs rw\n"), dataPaths); err != nil {
		t.Fatal(err)
	}
}

func TestHostStateRejectsCorruptOrUnrecognizedOwnership(t *testing.T) {
	base := State{Version: 1, ID: strings.Repeat("a", 32), Config: testConfig, Phase: "installing", Step: "planned", Packages: testPackages}
	for _, tc := range []struct {
		name   string
		mutate func(*State)
	}{
		{"version", func(s *State) { s.Version = 2 }},
		{"identity", func(s *State) { s.ID = "not-an-id" }},
		{"phase", func(s *State) { s.Phase = "unknown" }},
		{"purge", func(s *State) { s.PurgeApproved = true }},
		{"no-packages", func(s *State) { s.Packages = nil }},
		{"invalid-package", func(s *State) { s.Packages = []Package{{"--bad", "1"}} }},
		{"incomplete-config", func(s *State) { s.Configured = true }},
		{"account", func(s *State) { s.Accounts = map[string]string{"passwd:root": "root"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "host.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(path); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
	for _, data := range []string{`{"unknown":true}`, `null`, strings.Repeat(" ", 1024*1024+1)} {
		path := filepath.Join(t.TempDir(), "host.json")
		os.WriteFile(path, []byte(data), 0600)
		if _, err := ReadState(path); err == nil {
			t.Fatal("invalid JSON state accepted")
		}
	}
}
