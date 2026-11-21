package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const refreshService = "vamos-caddy-refresh.service"
const refreshTimer = "vamos-caddy-refresh.timer"
const systemUnitDir = "/etc/systemd/system/"
const refreshEnableLink = systemUnitDir + "timers.target.wants/" + refreshTimer

var refreshUnits = map[string]string{
	refreshService: `[Unit]
Description=Refresh the Vamos Caddy container
After=docker.service

[Service]
Type=oneshot
# Allow the executor's 20-minute deadline and its bounded recovery to finish.
TimeoutStartSec=22min
ExecStart=` + ExecutorPath + ` internal-caddy-refresh
`,
	refreshTimer: `[Unit]
Description=Daily Vamos Caddy refresh

[Timer]
OnCalendar=daily
RandomizedDelaySec=2h

[Install]
WantedBy=timers.target
`,
}

func refreshUnitBody(s *State, name string) string {
	return "# Managed by Vamos installation " + s.ID + "\n" + refreshUnits[name]
}

// An empty receipt means legacy/unowned, not permission to adopt matching files.
// Pending records our absence check before either unit is created.
func (m *Manager) checkRefreshFiles(s *State) error {
	for name := range refreshUnits {
		body := refreshUnitBody(s, name)
		for _, dir := range []string{systemUnitDir, "/run/systemd/system/", "/usr/local/lib/systemd/system/", "/usr/lib/systemd/system/"} {
			for _, suffix := range []string{"", ".d"} {
				p := m.path(dir + name + suffix)
				info, err := os.Lstat(p)
				owned := dir == systemUnitDir && suffix == "" && s.RefreshTimer != ""
				if errors.Is(err, os.ErrNotExist) && (!owned || s.RefreshTimer == "pending" || s.PurgeApproved) {
					continue
				}
				if err != nil {
					return err
				}
				if !owned || !refreshFileOwned(info) || info.Mode() != 0644 || info.Size() != int64(len(body)) {
					return fmt.Errorf("unexpected refresh unit or override: %s", p)
				}
				if err := rejectSymlink(p); err != nil {
					return err
				}
				b, err := os.ReadFile(p)
				if err != nil || string(b) != body {
					return fmt.Errorf("changed refresh unit: %s", p)
				}
			}
		}
	}
	link := m.path(refreshEnableLink)
	if err := rejectSymlink(filepath.Dir(link)); err != nil {
		return err
	}
	target, err := os.Readlink(link)
	if errors.Is(err, os.ErrNotExist) && (s.RefreshTimer != "installed" || s.PurgeApproved) {
		return nil
	}
	if err != nil || s.RefreshTimer == "" || (target != systemUnitDir+refreshTimer && target != "../"+refreshTimer) {
		return fmt.Errorf("unexpected timer enablement link: %s", link)
	}
	return nil
}

func (m *Manager) refreshNamesFree(ctx context.Context) error {
	if err := m.checkRefreshFiles(&State{}); err != nil {
		return err
	}
	for name := range refreshUnits {
		out, err := m.command(ctx, "systemctl", "show", "--property=LoadState", "--value", name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "not-found" {
			return fmt.Errorf("pre-existing unit %s", name)
		}
	}
	return nil
}

func (m *Manager) installRefreshTimer(ctx context.Context, s *State) error {
	if s.RefreshTimer == "installed" {
		return nil
	}
	if s.RefreshTimer == "" {
		if err := m.refreshNamesFree(ctx); err != nil {
			return err
		}
		s.RefreshTimer = "pending"
		if err := m.save(s); err != nil {
			return err
		}
	}
	if err := m.checkRefreshFiles(s); err != nil {
		return err
	}
	for name := range refreshUnits {
		body := refreshUnitBody(s, name)
		p := m.path(systemUnitDir + name)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			if err := atomicCreate(p, []byte(body), 0644); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := m.checkRefreshLoaded(ctx, refreshService, refreshTimer); err != nil {
		return err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", refreshTimer); err != nil {
		return err
	}
	if err := m.checkRefreshHealth(ctx); err != nil {
		return err
	}
	s.RefreshTimer = "installed"
	return m.save(s)
}

// File checks alone cannot see a transient/generated unit or a drop-in in a
// higher-priority systemd search directory. Check what PID 1 actually loaded.
func (m *Manager) checkRefreshLoaded(ctx context.Context, names ...string) error {
	for _, name := range names {
		out, err := m.command(ctx, "systemctl", "show", "--property=FragmentPath", "--value", name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != systemUnitDir+name {
			return fmt.Errorf("unexpected systemd fragment for %s", name)
		}
		out, err = m.command(ctx, "systemctl", "show", "--property=DropInPaths", "--value", name)
		if err != nil {
			return err
		}
		// System-wide defaults apply to all services (e.g. LXC guests). They
		// are not overrides of the Vamos unit and teardown never touches them.
		unitType := strings.TrimPrefix(filepath.Ext(name), ".") + ".d"
		for _, dropIn := range strings.Fields(string(out)) {
			switch filepath.Dir(dropIn) {
			case systemUnitDir + unitType, "/run/systemd/system/" + unitType, "/usr/lib/systemd/system/" + unitType, "/usr/local/lib/systemd/system/" + unitType:
			default:
				return fmt.Errorf("unexpected systemd drop-in for %s: %s", name, dropIn)
			}
		}
	}
	return nil
}

func (m *Manager) checkRefreshHealth(ctx context.Context) error {
	if err := m.checkRefreshLoaded(ctx, refreshService, refreshTimer); err != nil {
		return err
	}
	out, err := m.command(ctx, "systemctl", "is-enabled", refreshTimer)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "enabled" {
		return fmt.Errorf("%s is not enabled", refreshTimer)
	}
	_, err = m.command(ctx, "systemctl", "is-active", refreshTimer)
	return err
}

func (m *Manager) removeRefreshTimer(ctx context.Context, s *State) error {
	if s.RefreshTimer == "" {
		return nil
	}
	if err := m.checkRefreshFiles(s); err != nil {
		return err
	}
	if _, err := os.Lstat(m.path(systemUnitDir + refreshTimer)); err == nil {
		if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := m.checkRefreshLoaded(ctx, refreshTimer); err != nil {
			return err
		}
		if _, err := m.command(ctx, "systemctl", "stop", refreshTimer); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// systemctl disable removes *all* symlinks to a unit, including aliases
	// created by other administrators. Remove only the link we own.
	if err := os.Remove(m.path(refreshEnableLink)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for name := range refreshUnits {
		if err := os.Remove(m.path(systemUnitDir + name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	s.RefreshTimer = ""
	return m.save(s)
}

// App workers hold the shared side of the lifecycle lock for the entire mutation.
func (m *Manager) RefreshCaddy(ctx context.Context) (err error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return err
	}
	if s == nil || s.Phase != "ready" || s.RefreshTimer != "installed" {
		return errors.New("Caddy refresh requires a ready installation with a managed timer")
	}
	if err := m.verifyQueueIdle(); err != nil {
		return err
	}
	// Expose a bounded diagnostic to server status without copying command
	// output into the receipt or clearing unrelated lifecycle failures.
	const failure = "Daily Caddy refresh failed; inspect journalctl -u " + refreshService
	defer func() {
		if err != nil && s.LastError == "" {
			s.LastError = failure
			err = errors.Join(err, m.save(s))
		} else if err == nil && s.LastError == failure {
			s.LastError = ""
			err = m.save(s)
		}
	}()
	if err := m.checkRefreshHealth(ctx); err != nil {
		return err
	}
	if err := m.verifyFilesFor(s, true); err != nil {
		return err
	}
	return m.refreshCaddy(ctx, s)
}
