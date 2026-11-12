package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func checkBuildCache(output []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	found := false
	for {
		var row map[string]json.RawMessage
		if err := decoder.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("cannot verify Docker build cache: %w", err)
		}
		var kind string
		if err := json.Unmarshal(row["Type"], &kind); err != nil {
			return err
		}
		if kind != "Build Cache" {
			continue
		}
		found = true
		count, err := strconv.Atoi(strings.Trim(string(row["TotalCount"]), "\""))
		if err != nil || count != 0 {
			return errors.New("Docker build cache is nonempty or unrecognized; clean test-created cache explicitly before teardown")
		}
	}
	if !found {
		return errors.New("Docker did not report build cache usage; refusing data removal")
	}
	return nil
}

// Teardown requires an explicit data purge even though workloads always block it.
// The receipt and stable lock are retained; neither is evidence of active services.
func (m *Manager) Teardown(ctx context.Context, name string, purge bool) (_ *State, err error) {
	if !purge {
		return nil, errors.New("teardown requires explicit --purge to remove owned runtime data and certificates")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("no ownership manifest; refusing inferred cleanup")
	}
	if s.Config.Name != name {
		return nil, errors.New("server name does not match the host ownership manifest")
	}
	if err := m.rejectRetainedVolumes(); err != nil {
		return nil, err
	}
	if s.Phase == "removed" {
		return s, nil
	}
	if err := m.checkStateDirectory(); err != nil {
		return nil, err
	}
	if err := m.checkMounts(); err != nil {
		return nil, err
	}
	for _, path := range dataPaths {
		if err := rejectSymlink(m.path(path)); err != nil {
			return nil, err
		}
	}
	if !s.PurgeApproved {
		// An interrupted package transaction can have created service accounts
		// before configuration was recorded. Initial preflight proved them absent.
		if s.Accounts == nil {
			s.Accounts = map[string]string{}
			for _, key := range []string{"passwd:caddy", "group:caddy", "group:docker"} {
				parts := strings.Split(key, ":")
				value, err := m.account(ctx, parts[0], parts[1])
				if err != nil {
					return nil, err
				}
				if value != "" {
					fields := strings.Split(value, ":")
					if parts[0] == "passwd" && (len(fields) != 7 || fields[0] != "caddy" || fields[5] != "/var/lib/caddy" || (fields[6] != "/usr/sbin/nologin" && fields[6] != "/bin/false")) {
						return nil, errors.New("unexpected caddy account after interrupted setup")
					}
					if parts[0] == "group" && (len(fields) != 4 || fields[0] != parts[1] || fields[3] != "") {
						return nil, errors.New("unexpected service group after interrupted setup")
					}
				}
				s.Accounts[key] = value
			}
		}
		if s.Configured {
			if err := m.verifyFiles(s); err != nil {
				return nil, err
			}
		}
		for key, expected := range s.Accounts {
			parts := strings.Split(key, ":")
			if len(parts) != 2 {
				return nil, errors.New("invalid account ownership record")
			}
			actual, err := m.account(ctx, parts[0], parts[1])
			if err != nil {
				return nil, err
			}
			if actual != expected {
				return nil, fmt.Errorf("account %s changed; refusing removal", key)
			}
		}
		active, err := m.serviceRunning(ctx, "caddy.service")
		if err != nil {
			return nil, err
		}
		if active {
			if err := m.checkCaddyRuntime(ctx); err != nil {
				return nil, err
			}
		}
		entries, statErr := os.ReadDir(m.path("/var/lib/docker"))
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		if len(entries) > 0 {
			if err := m.workloads(ctx); err != nil {
				return nil, err
			}
		}
	} else {
		// A lost connection is not rollback. Recheck workloads if the daemon is
		// still active; after it is stopped/masked we can resume package cleanup.
		for _, unit := range []string{"docker.service", "caddy.service"} {
			active, err := m.serviceRunning(ctx, unit)
			if err != nil {
				return nil, err
			}
			if active {
				var err error
				if unit == "docker.service" {
					err = m.workloads(ctx)
				} else {
					err = m.checkCaddyRuntime(ctx)
				}
				if err != nil {
					return nil, err
				}
			}
		}
	}
	present, err := m.removalPlan(ctx, s)
	if err != nil {
		return nil, err
	}
	defer func() { m.recordError(s, err) }()
	s.Phase = "removing"
	s.PurgeApproved = true
	if err := m.step(s, "stop-services"); err != nil {
		return nil, err
	}
	if err := m.mask(ctx); err != nil {
		return nil, err
	}
	for _, unit := range units {
		active, err := m.serviceRunning(ctx, unit)
		if err != nil {
			return nil, err
		}
		if active {
			if _, err := m.command(ctx, "systemctl", "stop", unit); err != nil {
				return nil, err
			}
		}
	}
	if err := m.step(s, "purge-packages"); err != nil {
		return nil, err
	}
	if len(present) > 0 {
		args := append([]string{"-y", "-qq", "--no-auto-remove", "-o", "Dpkg::Use-Pty=0", "-o", "DPkg::Lock::Timeout=60", "purge"}, packageArgs(present, false)...)
		if _, err := m.command(ctx, "apt-get", args...); err != nil {
			return nil, err
		}
	}
	remaining, err := m.packages(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range s.Packages {
		if _, ok := remaining[p.Name]; ok {
			return nil, fmt.Errorf("owned package %s remains after purge", p.Name)
		}
	}
	if err := m.step(s, "remove-owned-data"); err != nil {
		return nil, err
	}
	// Only fixed paths proven absent on first setup can be removed. The caller
	// has explicitly approved runtime-data deletion and workloads were checked.
	if err := m.checkMounts(); err != nil {
		return nil, err
	}
	for _, path := range dataPaths {
		if err := rejectSymlink(m.path(path)); err != nil {
			return nil, err
		}
		if err := os.RemoveAll(m.path(path)); err != nil {
			return nil, err
		}
	}
	if s.Accounts["passwd:caddy"] != "" {
		value, err := m.account(ctx, "passwd", "caddy")
		if err != nil {
			return nil, err
		}
		if value != "" {
			if value != s.Accounts["passwd:caddy"] {
				return nil, errors.New("caddy account drifted during teardown")
			}
			if _, err := m.command(ctx, "userdel", "caddy"); err != nil {
				return nil, err
			}
		}
	}
	for _, group := range []string{"caddy", "docker"} {
		if s.Accounts["group:"+group] == "" {
			continue
		}
		value, err := m.account(ctx, "group", group)
		if err != nil {
			return nil, err
		}
		if value != "" {
			if value != s.Accounts["group:"+group] {
				return nil, fmt.Errorf("group %s drifted during teardown", group)
			}
			if _, err := m.command(ctx, "groupdel", group); err != nil {
				return nil, err
			}
		}
	}
	if err := m.unmask(ctx); err != nil {
		return nil, err
	}
	if err := m.removeAppReceipts(s); err != nil {
		return nil, err
	}
	s.Phase = "removed"
	if err := m.step(s, "complete"); err != nil {
		return nil, err
	}
	return s, nil
}
