package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) preflight(ctx context.Context, receipt bool) error {
	for _, path := range dataPaths {
		if err := rejectSymlink(m.path(path)); err != nil {
			return err
		}
		if _, err := os.Lstat(m.path(path)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pre-existing %s; unmanaged installations/data cannot be adopted", path)
		}
	}
	if !receipt {
		if _, err := os.Lstat(m.path("/var/lib/vamos")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("/var/lib/vamos already exists without a valid receipt; refusing adoption")
		}
	}
	for _, unit := range units {
		if _, err := os.Lstat(m.path("/etc/systemd/system/" + unit)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pre-existing service override for %s", unit)
		}
		if err := m.checkOverrides(unit); err != nil {
			return err
		}
		out, err := m.Run(ctx, "systemctl", []string{"show", "--property=LoadState", "--value", unit})
		if err != nil {
			return fmt.Errorf("cannot establish unit ownership for %s: %w", unit, err)
		}
		if strings.TrimSpace(string(out.Stdout)) != "not-found" {
			return fmt.Errorf("pre-existing unit %s (load state: %q)", unit, strings.TrimSpace(string(out.Stdout)))
		}
	}
	for _, path := range []string{"/usr/bin/docker", "/usr/local/bin/docker", "/snap/bin/docker", "/usr/local/bin/vamos", "/etc/vamos", "/etc/ssh/sshd_config.d/vamos.conf", "/etc/systemd/system/" + agentUnit} {
		if _, err := os.Lstat(m.path(path)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pre-existing executable %s", path)
		}
	}
	for _, key := range ownedAccounts {
		kind, name, _ := strings.Cut(key, ":")
		value, err := m.account(ctx, kind, name)
		if err != nil {
			return err
		}
		if value != "" {
			return fmt.Errorf("pre-existing %s account %s", kind, name)
		}
	}
	inventory, err := m.packages(ctx)
	if err != nil {
		return err
	}
	for name := range inventory {
		if name == "runc" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "containerd") || name == "moby-engine" {
			return fmt.Errorf("pre-existing runtime package %s; refusing adoption", name)
		}
	}
	if audit, err := m.command(ctx, "dpkg", "--audit"); err != nil {
		return fmt.Errorf("dpkg has pending or broken work; resolve it before setup (query error: %w)", err)
	} else if len(strings.TrimSpace(string(audit))) != 0 {
		return errors.New("dpkg has pending or broken work; resolve it before setup")
	}
	for _, port := range []string{"80", "443", "2019"} {
		out, err := m.command(ctx, "ss", "-H", "-ltn", "sport = :"+port)
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(out))) != 0 {
			return fmt.Errorf("TCP port %s is already in use", port)
		}
	}
	return nil
}

func (m *Manager) checkOverrides(unit string) error {
	for _, path := range []string{"/etc/systemd/system/" + unit, "/etc/systemd/system/" + unit + ".d", "/run/systemd/system/" + unit, "/run/systemd/system/" + unit + ".d"} {
		actual := m.path(path)
		if err := rejectSymlink(filepath.Dir(actual)); err != nil {
			return err
		}
		if target, err := os.Readlink(actual); err == nil && target == "/dev/null" && strings.HasPrefix(path, "/etc/") && !strings.HasSuffix(path, ".d") {
			continue
		}
		if _, err := os.Lstat(actual); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unexpected service override at %s", path)
		}
	}
	return nil
}

func (m *Manager) account(ctx context.Context, kind, name string) (string, error) {
	result, err := m.Run(ctx, "getent", []string{kind, name})
	if err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", nil
		}
		return "", fmt.Errorf("query %s %s: %w", kind, name, err)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

// workloads proves Docker holds nothing but Vamos's own resources. Before purge
// approval the owned Caddy container and image are still in service, so they
// are allowed by identity. containerd content blobs cannot be attributed to an
// image without walking manifests, so content is checked only once the owned
// ingress has been removed.
func (m *Manager) workloads(ctx context.Context, caddy *caddyContainer) error {
	allowed := map[string]bool{}
	if caddy != nil {
		allowed[caddy.ID] = true
		for _, ref := range caddy.imageRefs() {
			images, err := m.caddyImageIDs(ctx, ref)
			if err != nil {
				return fmt.Errorf("cannot verify Docker is unused; restore daemon health before teardown: %w", err)
			}
			for _, id := range images {
				allowed[id] = true
			}
		}
	}
	for _, args := range [][]string{{"container", "ls", "-aq", "--no-trunc"}, {"image", "ls", "-aq", "--no-trunc"}, {"volume", "ls", "-q"}} {
		out, err := m.command(ctx, "docker", args...)
		if err != nil {
			return fmt.Errorf("cannot verify Docker is unused; restore daemon health before teardown: %w", err)
		}
		for _, id := range strings.Fields(string(out)) {
			if !allowed[id] {
				return fmt.Errorf("Docker has %s resources; remove workloads explicitly before host teardown", args[0])
			}
		}
	}
	out, err := m.command(ctx, "docker", "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	for _, network := range strings.Fields(string(out)) {
		if network != "bridge" && network != "host" && network != "none" {
			return fmt.Errorf("Docker has custom network %q; refusing teardown", network)
		}
	}
	// Build caches also contain user data. A nonempty cache must be cleaned by
	// its operator, not by a global prune hidden in teardown.
	out, err = m.command(ctx, "docker", "system", "df", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	if err := checkBuildCache(out); err != nil {
		return err
	}
	namespaces, err := m.command(ctx, "ctr", "namespaces", "ls", "-q")
	if err != nil {
		return err
	}
	for _, namespace := range strings.Fields(string(namespaces)) {
		if namespace != "moby" && namespace != "plugins.moby" {
			return fmt.Errorf("containerd has unmanaged namespace %q; refusing teardown", namespace)
		}
		for _, resource := range []string{"containers", "images", "content"} {
			if caddy != nil && resource == "content" {
				continue
			}
			out, err := m.command(ctx, "ctr", "--namespace", namespace, resource, "ls", "-q")
			if err != nil {
				return err
			}
			for _, item := range strings.Fields(string(out)) {
				if caddy == nil || namespace != "moby" || !caddy.ownsContainerd(resource, item) {
					return fmt.Errorf("containerd has %s in %s; refusing teardown", resource, namespace)
				}
			}
		}
	}
	return nil
}

func (m *Manager) checkStateDirectory(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Dir(m.path(StatePath)))
	if err != nil {
		return err
	}
	s, err := ReadState(m.path(StatePath))
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("missing host ownership manifest")
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "host.json":
		case "apps":
			if _, err := m.appReceipts(s); err != nil {
				return err
			}
		case "queue":
			if err := m.verifyQueueIdle(); err != nil {
				return err
			}
		case "buildkit":
			if s.PurgeApproved {
				// The final receipt unlink may have completed before its empty
				// directory was removed. Only authorized purge may reconcile it.
				files, err := os.ReadDir(m.path("/var/lib/vamos/buildkit"))
				if err == nil && len(files) == 0 {
					continue
				}
			}
			if err := m.buildService().ReceiptGone(ctx, s.ID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected Vamos state %s; refusing removal", entry.Name())
		}
	}
	return nil
}

func (m *Manager) verifyQueueIdle() error {
	queueRoot := m.path("/var/lib/vamos/queue")
	if err := rejectSymlink(queueRoot); err != nil {
		return err
	}
	queueEntries, err := os.ReadDir(queueRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect coordinator queue: %w", err)
	}
	if len(queueEntries) != 1 || queueEntries[0].Name() != "operations" || !queueEntries[0].IsDir() {
		return errors.New("agent queue is nonempty; inspect or resolve recorded operations before teardown")
	}
	operations := filepath.Join(queueRoot, "operations")
	if err := rejectSymlink(operations); err != nil {
		return err
	}
	operationEntries, err := os.ReadDir(operations)
	if err != nil {
		return fmt.Errorf("cannot inspect coordinator queue operations: %w", err)
	}
	type queueRecord struct {
		OperationID string `json:"operation_id"`
		Phase       string `json:"phase"`
		ResolvedBy  string `json:"resolved_by"`
	}
	var records []queueRecord
	succeeded := make(map[string]bool)
	for _, entry := range operationEntries {
		if !entry.IsDir() && (strings.HasPrefix(entry.Name(), ".record-") || strings.HasPrefix(entry.Name(), ".payload-")) {
			continue
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return errors.New("agent queue has staged or unexpected entries; inspect or resolve operations before teardown")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
			return errors.New("agent queue record is unreadable; inspect or resolve recorded operations before teardown")
		}
		data, err := os.ReadFile(filepath.Join(operations, entry.Name()))
		if err != nil {
			return errors.New("agent queue record is unreadable; inspect or resolve recorded operations before teardown")
		}
		var record queueRecord
		dec := json.NewDecoder(bytes.NewReader(data))
		if err := dec.Decode(&record); err != nil {
			return errors.New("agent queue record is invalid; inspect or resolve recorded operations before teardown")
		}
		var trailing any
		if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) || record.OperationID == "" || record.OperationID+".json" != entry.Name() {
			return errors.New("agent queue record is invalid; inspect or resolve recorded operations before teardown")
		}
		if record.Phase == "succeeded" && record.OperationID != "" {
			succeeded[record.OperationID] = true
		}
		records = append(records, record)
	}
	for _, record := range records {
		switch record.Phase {
		case "succeeded", "failed", "cancelled":
		case "uncertain":
			if record.ResolvedBy == "" || !succeeded[record.ResolvedBy] {
				return errors.New("agent queue has unfinished or uncertain operations; inspect or resolve them before teardown")
			}
		default:
			return errors.New("agent queue has unfinished or uncertain operations; inspect or resolve them before teardown")
		}
	}
	return nil
}
