package lifecycle

import (
	"context"
	"errors"
	"os"

	"github.com/nchapman/vamos/internal/firewall"
)

func (m *Manager) firewallManager() *firewall.Manager {
	return &firewall.Manager{Root: m.Root, Run: firewall.Runner(m.Run)}
}

func (m *Manager) hasFirewallReceipt() (bool, error) {
	for _, path := range []string{"/var/lib/vamos/firewall.json", "/var/lib/vamos/firewall.retired.json", "/var/lib/vamos/firewall.confirmed"} {
		if err := rejectSymlink(m.path(path)); err != nil {
			return false, err
		}
		_, err := os.Lstat(m.path(path))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (m *Manager) checkFirewall(ctx context.Context, hostID string) error {
	owned, err := m.hasFirewallReceipt()
	if err != nil || !owned {
		return err
	}
	status, err := m.firewallManager().Status(ctx, hostID)
	if err != nil {
		return err
	}
	if status.Degraded {
		return errors.New("managed firewall has drifted; inspect server firewall status before continuing")
	}
	if status.Pending {
		return errors.New("managed firewall confirmation is pending")
	}
	return nil
}

func (m *Manager) checkFirewallForUpgrade(ctx context.Context, hostID string) error {
	owned, err := m.hasFirewallReceipt()
	if err != nil || !owned {
		return err
	}
	return m.firewallManager().CheckUpgradeable(ctx, hostID)
}

func (m *Manager) upgradeFirewall(ctx context.Context, hostID string) (string, error) {
	owned, err := m.hasFirewallReceipt()
	if err != nil || !owned {
		return "", err
	}
	return m.firewallManager().Upgrade(ctx, hostID)
}

func (m *Manager) restoreFirewall(ctx context.Context, hostID, oldScript string) error {
	if oldScript == "" {
		return nil
	}
	return m.firewallManager().RestoreScript(ctx, hostID, oldScript)
}

// Firewall uses the same installation identity and host mutation lock as other
// administrative operations. Confirmation is a separate request so the client
// must establish a new SSH connection through the staged policy.
func (m *Manager) Firewall(ctx context.Context, cfg Config, operation, token string) (firewall.Status, error) {
	var empty firewall.Status
	switch operation {
	case "enable", "disable", "status":
		if token != "" {
			return empty, errors.New("only firewall confirmation accepts a token")
		}
	case "confirm":
		if token == "" {
			return empty, errors.New("firewall confirmation requires a token")
		}
	default:
		return empty, errors.New("unsupported firewall operation")
	}
	if err := ValidateConfig(cfg); err != nil {
		return empty, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return empty, err
	}
	defer unlock()
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return empty, err
	}
	if s == nil || s.Phase != "ready" || s.Config != cfg {
		return empty, errors.New("server is not a ready installation with the requested identity")
	}
	if operation == "enable" {
		// Like upgrade, enabling protection must allow deployed routes while
		// preserving the managed Caddy base and all other ownership checks.
		if err := m.verifyFilesFor(s, true); err != nil {
			return empty, err
		}
		if err := m.verifyQueueIdle(); err != nil {
			return empty, err
		}
	}
	f := m.firewallManager()
	switch operation {
	case "enable":
		return f.Enable(ctx, s.ID)
	case "confirm":
		return f.Confirm(ctx, s.ID, token)
	case "disable":
		return f.Disable(ctx, s.ID)
	default:
		return f.Status(ctx, s.ID)
	}
}
