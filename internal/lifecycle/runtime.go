package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"
)

// is-active legitimately exits 3/4 for inactive/missing units. Other errors
// (including cancellation or a failed system bus query) are not evidence of rest.
func (m *Manager) serviceRunning(ctx context.Context, unit string) (bool, error) {
	result, err := m.Run(ctx, "systemctl", []string{"is-active", unit})
	state := strings.TrimSpace(string(result.Stdout))
	if err != nil {
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || (exit.ExitCode() != 3 && exit.ExitCode() != 4) {
			return false, fmt.Errorf("cannot establish service state for %s: %w", unit, err)
		}
	}
	switch state {
	case "active", "reloading":
		if err != nil {
			return false, fmt.Errorf("inconsistent service state for %s: %w", unit, err)
		}
		return true, nil
	case "activating", "deactivating":
		return true, nil
	case "inactive", "failed", "unknown":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized service state for %s: %q", unit, state)
	}
}

func readCaddyConfig(ctx context.Context) ([]byte, error) {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:2019/config/", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Caddy admin API returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("Caddy runtime config exceeds 1 MiB")
	}
	return data, nil
}

func (m *Manager) checkCaddyRuntime(ctx context.Context) error {
	read := m.CaddyConfig
	if read == nil {
		read = readCaddyConfig
	}
	actual, err := read(ctx)
	if err != nil {
		return fmt.Errorf("cannot verify Caddy runtime config: %w", err)
	}
	name, args := CaddyCommand("adapt", "--config", caddyfilePath, "--adapter", "caddyfile")
	expected, err := m.command(ctx, name, args...)
	if err != nil {
		return err
	}
	var a, b any
	if err := json.Unmarshal(actual, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(expected, &b); err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("Caddy runtime configuration differs from the managed file; refusing to discard API-loaded routes")
	}
	return nil
}

func (m *Manager) checkMounts() error {
	var data []byte
	var err error
	if m.MountInfo != nil {
		data, err = m.MountInfo()
	} else {
		data, err = os.ReadFile("/proc/self/mountinfo")
	}
	if err != nil {
		return fmt.Errorf("cannot verify owned paths are not mounted: %w", err)
	}
	// Completed app receipts and coordinator directories are also explicitly
	// removed during host purge, so verify none of them overlap mount points.
	return checkMountInfo(data, append(append([]string{}, dataPaths...), "/var/lib/vamos/apps", "/var/lib/vamos/queue", "/etc/vamos"))
}

func checkMountInfo(data []byte, paths []string) error {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) < 6 {
			return errors.New("invalid mount inventory")
		}
		mount := unescape.Replace(fields[4])
		for _, path := range paths {
			if mount == path || strings.HasPrefix(mount, path+"/") {
				return fmt.Errorf("mount at %s overlaps owned data; unmount it explicitly before teardown", mount)
			}
		}
	}
	return nil
}
