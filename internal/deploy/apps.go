package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/appstate"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// MaxApps bounds both directory enumeration and the union of registered apps.
// Inventory never silently truncates; the encoded response has a separate 64 KiB limit.
const MaxApps = 256

type AppList struct {
	Entries []AppEntry `json:"entries"`
}

// AppEntry describes recorded metadata, not live container or volume existence.
// Services includes retained volume service names, even after destruction.
type AppEntry struct {
	App             string   `json:"app"`
	Phase           string   `json:"phase"`
	Services        []string `json:"services"`
	ServiceCount    int      `json:"service_count"`
	RetainedSecrets bool     `json:"retained_secrets"`
	VolumeRecords   int      `json:"volume_records"`
	RetainedVolumes bool     `json:"retained_volumes"`
}

func inventoryPath(path string, directory bool, private bool) error {
	if err := safe(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || !inventoryOwned(info) {
		return errors.New("app inventory requires executor-owned directories and regular files")
	}
	if runtime.GOOS != "windows" {
		mode := os.FileMode(0600)
		if directory {
			mode = 0700
		}
		if info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm() != mode) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("unsafe app inventory permissions")
		}
	}
	return nil
}

func inventoryNames(dir string, directories bool) ([]string, error) {
	if err := inventoryPath(dir, true, true); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := inventoryPath(filepath.Dir(dir), true, false); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(MaxApps + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > MaxApps {
		return nil, fmt.Errorf("app inventory exceeds %d entries per store", MaxApps)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !directories {
			if strings.HasPrefix(name, ".secret-") || strings.HasPrefix(name, ".volume-") {
				continue
			}
			if !strings.HasSuffix(name, ".json") {
				return nil, errors.New("unexpected app inventory metadata file")
			}
			name = strings.TrimSuffix(name, ".json")
		}
		if !appstate.ValidName(name) {
			return nil, errors.New("invalid app inventory metadata name")
		}
		if err := inventoryPath(filepath.Join(dir, entry.Name()), directories, true); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (m *Manager) listApps(ctx context.Context, hostID string, host lifecycle.Config) (*AppList, error) {
	// Execute has already validated the host journal's schema and identity. Inventory
	// also checks its filesystem ownership before trusting it as an enumeration root.
	if err := inventoryPath(m.path(lifecycle.StatePath), false, true); err != nil {
		return nil, err
	}
	if err := inventoryPath(filepath.Dir(m.path(lifecycle.StatePath)), true, false); err != nil {
		return nil, err
	}
	candidates := map[string]bool{}
	for _, source := range []struct {
		path string
		dirs bool
	}{
		{m.path("/var/lib/vamos/apps"), true},
		{m.path("/var/lib/vamos/secrets"), false},
		{m.path("/var/lib/vamos/volumes"), false},
	} {
		names, err := inventoryNames(source.path, source.dirs)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			candidates[name] = true
		}
		if len(candidates) > MaxApps {
			return nil, fmt.Errorf("app inventory exceeds %d apps", MaxApps)
		}
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	out := &AppList{Entries: []AppEntry{}}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := AppEntry{App: name, Phase: "staged", Services: []string{}}
		services := map[string]bool{}
		var j *journal
		if _, err := os.Lstat(m.dir(name)); err == nil {
			if err := inventoryPath(m.dir(name), true, true); err != nil {
				return nil, err
			}
			if err := inventoryPath(filepath.Join(m.dir(name), "state.json"), false, true); err != nil {
				return nil, fmt.Errorf("app %s journal: %w", name, err)
			}
			var err error
			j, err = m.load(name)
			// Do not echo decoder diagnostics: journals can contain sensitive configuration.
			if err != nil {
				return nil, fmt.Errorf("app %s has invalid journal metadata", name)
			}
			if j.HostID != hostID || (j.Phase != "destroyed" && j.URL != "https://"+name+"."+host.Domain) {
				return nil, fmt.Errorf("app %s has foreign journal metadata", name)
			}
			entry.Phase = j.Phase
			if j.Config != nil {
				for service := range j.Config.Services {
					services[service] = true
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		secretState, err := m.secretStore().Load(name, hostID)
		if err != nil {
			return nil, fmt.Errorf("app %s has invalid secret metadata", name)
		}
		entry.RetainedSecrets = len(secretState.List()) != 0
		volumeState, err := m.volumeStore().Load(name, hostID)
		if err != nil {
			return nil, fmt.Errorf("app %s has invalid volume metadata", name)
		}
		if volumeState != nil {
			entry.VolumeRecords = len(volumeState.Records)
			for service := range volumeState.Records {
				if !services[service] {
					entry.RetainedVolumes = true
				}
				services[service] = true
			}
			if j == nil && entry.VolumeRecords > 0 {
				entry.Phase = "retained"
			}
		}
		for service := range services {
			entry.Services = append(entry.Services, service)
		}
		sort.Strings(entry.Services)
		entry.ServiceCount = len(entry.Services)
		out.Entries = append(out.Entries, entry)
	}
	encoded, err := json.Marshal(Response{Version: 1, Apps: out})
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > 64<<10 {
		return nil, errors.New("app inventory response exceeds 64 KiB; no entries returned")
	}
	return out, nil
}
