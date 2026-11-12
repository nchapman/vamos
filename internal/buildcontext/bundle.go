package buildcontext

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"

	"github.com/nchapman/soar/internal/config"
)

const manifestLimit = 64 * 1024

type bundleEntry struct {
	Key    string `json:"key"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}
type bundleManifest struct {
	Version int           `json:"version"`
	Entries []bundleEntry `json:"entries"`
}

func normalizedBuild(b config.Build) config.Build {
	if b.Context == "" {
		b.Context = "."
	}
	if b.Dockerfile == "" {
		b.Dockerfile = "Dockerfile"
	}
	// Config paths use slash syntax on every client OS. Filesystem-native
	// cleaning would produce different bundle IDs on Windows and Linux.
	b.Context = path.Clean(b.Context)
	b.Dockerfile = path.Clean(b.Dockerfile)
	return b
}

// BuildKey identifies only the normalized build specification, not runtime settings.
func BuildKey(b config.Build) string {
	data, _ := json.Marshal(normalizedBuild(b))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// Builds returns the distinct normalized build specifications in an app.
func Builds(a *config.App) map[string]config.Build {
	result := make(map[string]config.Build)
	if a != nil {
		for _, s := range a.Services {
			if s.Build != nil {
				b := normalizedBuild(*s.Build)
				result[BuildKey(b)] = b
			}
		}
	}
	return result
}

// Bundle archives each distinct build exactly once using the supplied archiver.
// The entire envelope, including its bounded manifest, fits the transport cap.
func Bundle(filename string, a *config.App, archive func(string, config.Build) ([]byte, error)) ([]byte, error) {
	builds := Builds(a)
	keys := make([]string, 0, len(builds))
	for k := range builds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if a == nil || len(a.Services) == 0 || (len(keys) > 0 && archive == nil) {
		return nil, fmt.Errorf("bundle requires builds and an archiver")
	}
	m := bundleManifest{Version: 1, Entries: []bundleEntry{}}
	payload := bytes.Buffer{}
	for _, k := range keys {
		data, err := archive(filename, builds[k])
		if err != nil {
			return nil, err
		}
		if len(data) > MaxBytes-4-payload.Len() {
			return nil, fmt.Errorf("bundle exceeds %d byte limit", MaxBytes)
		}
		sum := sha256.Sum256(data)
		m.Entries = append(m.Entries, bundleEntry{k, len(data), hex.EncodeToString(sum[:])})
		payload.Write(data)
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(manifest) > manifestLimit || 4+len(manifest)+payload.Len() > MaxBytes {
		return nil, fmt.Errorf("bundle exceeds size limit")
	}
	out := make([]byte, 4, 4+len(manifest)+payload.Len())
	binary.BigEndian.PutUint32(out, uint32(len(manifest)))
	out = append(out, manifest...)
	out = append(out, payload.Bytes()...)
	return out, nil
}

// ReadBundle strictly validates the envelope and each archive before returning
// any payload. Manifest bytes must use the canonical encoding emitted by Bundle.
func ReadBundle(data []byte, a *config.App) (map[string][]byte, error) {
	fail := func() (map[string][]byte, error) { return nil, fmt.Errorf("invalid or corrupt build bundle") }
	if len(data) < 4 || len(data) > MaxBytes {
		return fail()
	}
	n := int(binary.BigEndian.Uint32(data[:4]))
	if n <= 0 || n > manifestLimit || n > len(data)-4 {
		return fail()
	}
	var m bundleManifest
	dec := json.NewDecoder(bytes.NewReader(data[4 : 4+n]))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || dec.Decode(new(any)) != io.EOF || m.Version != 1 {
		return fail()
	}
	canonical, _ := json.Marshal(m)
	if !bytes.Equal(canonical, data[4:4+n]) {
		return fail()
	}
	builds := Builds(a)
	if a == nil || len(a.Services) == 0 || m.Entries == nil || len(m.Entries) != len(builds) {
		return fail()
	}
	result := make(map[string][]byte)
	rest := data[4+n:]
	for _, e := range m.Entries {
		if _, ok := builds[e.Key]; !ok {
			return fail()
		}
		if _, ok := result[e.Key]; ok {
			return fail()
		}
		if e.Size < 0 || e.Size > len(rest) {
			return fail()
		}
		payload := rest[:e.Size]
		rest = rest[e.Size:]
		sum := sha256.Sum256(payload)
		if e.SHA256 != hex.EncodeToString(sum[:]) {
			return fail()
		}
		result[e.Key] = payload
	}
	if len(rest) != 0 {
		return fail()
	}
	return result, nil
}

// Select returns a shallow configuration copy containing only the selected service.
// Callers validate the full project before selecting operation inputs.
func Select(a *config.App, service string) (*config.App, error) {
	if a == nil {
		return nil, fmt.Errorf("missing app configuration")
	}
	if service == "" {
		return a, nil
	}
	s, ok := a.Services[service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	selected := *a
	selected.Services = map[string]config.Service{service: s}
	return &selected, nil
}
