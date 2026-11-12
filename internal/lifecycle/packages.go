package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

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

func parseInstallPlan(output []byte, baseline map[string]installedPackage) ([]Package, error) {
	var packages []Package
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
		packages = append(packages, Package{Name: parts[1], Version: parts[2]})
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return packages, nil
}

func packageArgs(packages []Package, pin bool) []string {
	args := make([]string, 0, len(packages))
	for _, p := range packages {
		value := p.Name
		if pin {
			value += "=" + p.Version
		}
		args = append(args, value)
	}
	return args
}

func checkVersions(s *State, installed map[string]installedPackage, requireAll bool) error {
	for _, p := range s.Packages {
		actual, exists := installed[p.Name]
		if !exists {
			if requireAll {
				return fmt.Errorf("owned package %s is missing; retry setup to repair", p.Name)
			}
			continue
		}
		if actual.Version != p.Version {
			return fmt.Errorf("owned package %s changed version; refusing to overwrite or remove it", p.Name)
		}
		if requireAll && actual.Status != "installed" {
			return fmt.Errorf("package %s is %s; inspect dpkg health before retrying", p.Name, actual.Status)
		}
	}
	return nil
}

func (m *Manager) installPackages(ctx context.Context, s *State) error {
	installed, err := m.packages(ctx)
	if err != nil {
		return err
	}
	if err := checkVersions(s, installed, false); err != nil {
		return err
	}
	args := append([]string{"--simulate", "--no-install-recommends", "--no-remove", "install"}, packageArgs(s.Packages, true)...)
	plan, err := m.command(ctx, "apt-get", args...)
	if err != nil {
		return err
	}
	// Existing owned packages are allowed, but no unrecorded dependency changes.
	baseline := make(map[string]installedPackage)
	for name, p := range installed {
		baseline[name] = p
	}
	allowed := map[string]string{}
	for _, p := range s.Packages {
		delete(baseline, p.Name)
		allowed[p.Name] = p.Version
	}
	planned, err := parseInstallPlan(plan, baseline)
	if err != nil {
		return err
	}
	for _, p := range planned {
		if allowed[p.Name] != p.Version {
			return fmt.Errorf("APT plan drift for %s; refusing an unrecorded package transaction", p.Name)
		}
	}
	if len(planned) != 0 {
		args = append([]string{"-y", "-qq", "--no-install-recommends", "--no-remove", "-o", "Dpkg::Use-Pty=0", "-o", "DPkg::Lock::Timeout=60", "install"}, packageArgs(s.Packages, true)...)
		if _, err := m.command(ctx, "apt-get", args...); err != nil {
			return err
		}
	}
	installed, err = m.packages(ctx)
	if err != nil {
		return err
	}
	return checkVersions(s, installed, true)
}

func checkPurgePlan(output []byte, owned []Package) error {
	allowed := map[string]bool{}
	for _, p := range owned {
		allowed[p.Name] = true
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Inst ") {
			return errors.New("APT would install packages during teardown")
		}
		if strings.HasPrefix(line, "Remv ") || strings.HasPrefix(line, "Purg ") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !allowed[fields[1]] {
				return fmt.Errorf("APT would remove an unowned package: %q", line)
			}
		}
	}
	return nil
}

func (m *Manager) removalPlan(ctx context.Context, s *State) ([]Package, error) {
	installed, err := m.packages(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkVersions(s, installed, false); err != nil {
		return nil, err
	}
	var present []Package
	for _, p := range s.Packages {
		if _, ok := installed[p.Name]; ok {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return nil, nil
	}
	args := append([]string{"--simulate", "--no-auto-remove", "purge"}, packageArgs(present, false)...)
	out, err := m.command(ctx, "apt-get", args...)
	if err != nil {
		return nil, err
	}
	if err := checkPurgePlan(out, s.Packages); err != nil {
		return nil, err
	}
	return present, nil
}
