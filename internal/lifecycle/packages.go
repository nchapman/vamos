package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// aptLockTimeoutOption bounds APT's own dpkg lock acquisition, not the
// installation itself. The caller context still bounds the entire command.
// This does not cover apt-get update's separate lists lock.
const aptLockTimeoutOption = "DPkg::Lock::Timeout=60"

func aptMutationArgs(args ...string) []string {
	return append([]string{"-y", "-qq", "-o", "Dpkg::Use-Pty=0", "-o", aptLockTimeoutOption}, args...)
}

// aptLockDiagnostic only classifies lock acquisition failures, not generic
// dpkg errors (which may require a different recovery procedure).
func aptLockDiagnostic(args []string, output []byte) string {
	text := strings.ToLower(string(output))
	locked := strings.Contains(text, "could not get lock") ||
		strings.Contains(text, "unable to acquire the dpkg frontend lock") ||
		strings.Contains(text, "unable to lock directory") ||
		strings.Contains(text, "is another process using it?")
	if !locked {
		return ""
	}
	advice := "wait for cloud-init or unattended-upgrades (or another package manager) to finish, then retry; do not delete lock files or kill package processes"
	if contains(args, "update") {
		return "apt-get update could not acquire its separate package-list lock; automatic waiting for this lock is not implemented; " + advice
	}
	if contains(args, aptLockTimeoutOption) && !contains(args, "--simulate") {
		return "APT could not acquire a package-manager lock (dpkg lock waiting is limited to 60 seconds, or less if the caller cancels); " + advice
	}
	return "APT could not acquire a package-manager lock; " + advice
}

var installLine = regexp.MustCompile(`^Inst ([^ ]+)(?: \[[^]]+\])? \(([^ ]+) `)

type installedPackage struct{ Status, Version string }

func (m *Manager) packages(ctx context.Context) (map[string]installedPackage, error) {
	out, err := m.command(ctx, "dpkg-query", "-W", "-f=${Package}\t${db:Status-Status}\t${Version}\n")
	if err != nil {
		return nil, err
	}
	result := map[string]installedPackage{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || !packageRE.MatchString(parts[0]) {
			return nil, errors.New("unexpected dpkg package inventory")
		}
		if parts[1] != "not-installed" {
			result[parts[0]] = installedPackage{parts[1], parts[2]}
		}
	}
	return result, nil
}

// parseInstallPlan returns the fresh package names an APT simulation would
// install. Upgrades, removals and changes to baseline packages are refused.
func parseInstallPlan(output []byte, baseline map[string]installedPackage) ([]string, error) {
	var packages []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Remv ") || strings.HasPrefix(line, "Purg ") {
			return nil, errors.New("APT proposes package removal during setup")
		}
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		// A bracketed current version is an upgrade even when APT qualifies
		// the architecture differently from dpkg-query's package name.
		if strings.Contains(strings.SplitN(line, " (", 2)[0], " [") {
			return nil, errors.New("APT proposes upgrading an existing package")
		}
		parts := installLine.FindStringSubmatch(line)
		if parts == nil || !packageRE.MatchString(parts[1]) || !versionRE.MatchString(parts[2]) {
			return nil, errors.New("unrecognized APT installation plan")
		}
		if _, exists := baseline[parts[1]]; exists {
			return nil, fmt.Errorf("APT would modify pre-existing package %s; refusing adoption or upgrade", parts[1])
		}
		if seen[parts[1]] {
			return nil, errors.New("duplicate package in APT plan")
		}
		seen[parts[1]] = true
		packages = append(packages, parts[1])
	}
	sort.Strings(packages)
	return packages, nil
}

// checkPackages verifies owned packages by name only. Ubuntu's unattended
// upgrades deliberately own their versions, so a changed version is expected.
func checkPackages(s *State, installed map[string]installedPackage, requireAll bool) error {
	for _, name := range s.Packages {
		actual, exists := installed[name]
		if !exists {
			if requireAll {
				return fmt.Errorf("owned package %s is missing; retry setup to repair", name)
			}
			continue
		}
		if requireAll && actual.Status != "installed" {
			return fmt.Errorf("package %s is %s; inspect dpkg health before retrying", name, actual.Status)
		}
	}
	return nil
}

// installPackages installs the owned names from the current archive. It never
// upgrades packages that are already present: that is Ubuntu's job. Fresh
// dependencies a newer archive introduces are recorded before installation.
func (m *Manager) installPackages(ctx context.Context, s *State) error {
	installed, err := m.packages(ctx)
	if err != nil {
		return err
	}
	if err := checkPackages(s, installed, false); err != nil {
		return err
	}
	installArgs := append([]string{"--no-install-recommends", "--no-remove", "--no-upgrade", "install"}, s.Packages...)
	plan, err := m.command(ctx, "apt-get", append([]string{"--simulate"}, installArgs...)...)
	if err != nil {
		return err
	}
	baseline := make(map[string]installedPackage)
	for name, p := range installed {
		baseline[name] = p
	}
	for _, name := range s.Packages {
		delete(baseline, name)
	}
	planned, err := parseInstallPlan(plan, baseline)
	if err != nil {
		return err
	}
	added := false
	for _, name := range planned {
		if !contains(s.Packages, name) {
			s.Packages = append(s.Packages, name)
			added = true
		}
	}
	if added {
		sort.Strings(s.Packages)
		if err := m.save(s); err != nil {
			return err
		}
	}
	if len(planned) != 0 {
		args := aptMutationArgs(installArgs...)
		if _, err := m.command(ctx, "apt-get", args...); err != nil {
			return err
		}
	}
	installed, err = m.packages(ctx)
	if err != nil {
		return err
	}
	return checkPackages(s, installed, true)
}

func checkPurgePlan(output []byte, owned []string) error {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Inst ") {
			return errors.New("APT would install packages during teardown")
		}
		if strings.HasPrefix(line, "Remv ") || strings.HasPrefix(line, "Purg ") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !contains(owned, fields[1]) {
				return fmt.Errorf("APT would remove an unowned package: %q", line)
			}
		}
	}
	return nil
}

func (m *Manager) removalPlan(ctx context.Context, s *State) ([]string, error) {
	installed, err := m.packages(ctx)
	if err != nil {
		return nil, err
	}
	var present []string
	for _, name := range s.Packages {
		if _, ok := installed[name]; ok {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return nil, nil
	}
	args := append([]string{"--simulate", "--no-auto-remove", "purge"}, present...)
	out, err := m.command(ctx, "apt-get", args...)
	if err != nil {
		return nil, err
	}
	if err := checkPurgePlan(out, s.Packages); err != nil {
		return nil, err
	}
	return present, nil
}
