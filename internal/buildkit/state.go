package buildkit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const Image = "moby/buildkit:v0.27.1@sha256:1e110c71d389d6d24f67b9438e2f7b8da749a6ff407b22a1631e025c95599368"
const bridge = "vamos-build0"

var hostPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errInfra = errors.New("buildkit infrastructure validation or operation failed")

type receipt struct {
	Version                                            int
	Host, Nonce, Snapshotter                           string
	Container, Network, Volume, Builder, Bridge, Image string
	ContainerID, NetworkID, ImageID                    string
	ImageEnv                                           []string
	ImageLabels                                        map[string]string
	IPAM                                               ipam
	VolumeCreated, VolumeMountpoint                    string
}

func intent(host, snapshotter string) receipt {
	return receipt{Version: 1, Host: host, Snapshotter: snapshotter, Container: "vamos-buildkit-" + host, Network: "vamos-buildkit-" + host + "-net", Volume: "vamos-buildkit-" + host + "-cache", Builder: "vamos-" + host, Bridge: bridge, Image: Image}
}
func (s receipt) labels(role string) map[string]string {
	return map[string]string{"org.vamos.buildkit.host": s.Host, "org.vamos.buildkit.nonce": s.Nonce, "org.vamos.buildkit.role": role}
}
func strictJSON(b []byte, out any) error {
	if uniqueKeys(json.NewDecoder(bytes.NewReader(b)), 0) != nil {
		return errInfra
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errInfra
	}
	if d.Decode(new(any)) != io.EOF {
		return errInfra
	}
	return nil
}

// Reject duplicate (including case-aliased struct) fields rather than relying
// on encoding/json's last-value-wins behaviour for durable ownership data.
func uniqueKeys(d *json.Decoder, depth int) error {
	if depth > 100 {
		return errInfra
	}
	token, err := d.Token()
	if err != nil {
		return errInfra
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errInfra
			}
			name, ok := key.(string)
			if !ok {
				return errInfra
			}
			name = strings.ToLower(name)
			if seen[name] {
				return errInfra
			}
			seen[name] = true
			if uniqueKeys(d, depth+1) != nil {
				return errInfra
			}
		}
	case '[':
		for d.More() {
			if uniqueKeys(d, depth+1) != nil {
				return errInfra
			}
		}
	default:
		return errInfra
	}
	_, err = d.Token()
	if err != nil {
		return errInfra
	}
	return nil
}

// Check every existing path component before use. The root worker and its lock
// are trusted; this is not a defence against a concurrently malicious root.
func safeDir(path string, create bool) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return errInfra
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) && create {
			if os.Mkdir(cur, 0700) != nil || syncDir(filepath.Dir(cur)) != nil {
				return errInfra
			}
			st, err = os.Lstat(cur)
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errInfra
		}
	}
	return nil
}
func privateFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !ownedFile(st) || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
		return nil, errInfra
	}
	return os.ReadFile(path)
}
func (m Manager) state(host string) (receipt, string, error) {
	if !hostPattern.MatchString(host) || (m.Snapshotter != "" && m.Snapshotter != "auto" && m.Snapshotter != "native") {
		return receipt{}, "", errInfra
	}
	root := m.Root
	if root == "" {
		root = "/"
	}
	if !filepath.IsAbs(root) {
		return receipt{}, "", errInfra
	}
	dir := filepath.Join(root, "var/lib/vamos/buildkit")
	if err := safeDir(filepath.Dir(dir), true); err != nil {
		return receipt{}, "", err
	}
	st, err := os.Lstat(dir)
	fresh := os.IsNotExist(err)
	if fresh {
		if os.Mkdir(dir, 0700) != nil || syncDir(filepath.Dir(dir)) != nil {
			return receipt{}, "", errInfra
		}
	} else if err != nil || !ownedFile(st) || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return receipt{}, "", errInfra
	}
	if safeDir(dir, false) != nil {
		return receipt{}, "", errInfra
	}
	b, err := privateFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) && fresh {
		snap := m.Snapshotter
		if snap == "" {
			snap = "auto"
		}
		if snap != "auto" && snap != "native" {
			return receipt{}, "", errInfra
		}
		s := intent(host, snap)
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return receipt{}, "", errInfra
		}
		s.Nonce = hex.EncodeToString(nonce)
		if save(dir, s) != nil {
			return receipt{}, "", errInfra
		}
		return s, dir, nil
	}
	if err != nil {
		return receipt{}, "", errInfra
	}
	var s receipt
	if strictJSON(b, &s) != nil {
		return s, "", errInfra
	}
	want := intent(host, s.Snapshotter)
	if s.Version != 1 || s.Host != host || !hostPattern.MatchString(s.Nonce) || (s.Snapshotter != "auto" && s.Snapshotter != "native") || (m.Snapshotter != "" && m.Snapshotter != s.Snapshotter) || s.Container != want.Container || s.Network != want.Network || s.Volume != want.Volume || s.Builder != want.Builder || s.Bridge != bridge || s.Image != Image {
		return s, "", errInfra
	}
	if (s.ContainerID != "" && !idPattern.MatchString(s.ContainerID)) || (s.NetworkID != "" && !idPattern.MatchString(s.NetworkID)) || (s.ImageID != "" && (!strings.HasPrefix(s.ImageID, "sha256:") || !idPattern.MatchString(strings.TrimPrefix(s.ImageID, "sha256:")))) {
		return s, "", errInfra
	}
	if s.ContainerID != "" && (s.NetworkID == "" || s.VolumeCreated == "" || s.ImageID == "") {
		return s, "", errInfra
	}
	return s, dir, nil
}
func save(dir string, s receipt) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errInfra
	}
	f, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return errInfra
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errInfra
	}
	if os.Rename(name, filepath.Join(dir, "state.json")) != nil {
		return errInfra
	}
	return syncDir(dir)
}
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return errInfra
	}
	defer d.Close()
	if d.Sync() != nil {
		return errInfra
	}
	return nil
}
