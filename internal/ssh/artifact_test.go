package ssh

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/buildmeta"
)

func linuxELF() []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], 183)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	return data
}

func TestArtifactPairingAndArchitecture(t *testing.T) {
	stamp, _ := buildmeta.Encode(strings.Repeat("a", 64), "dev")
	other, _ := buildmeta.Encode(strings.Repeat("b", 64), "dev")
	for _, tt := range []struct {
		name, arch, marker string
		ok                 bool
	}{
		{"matching override", "arm64", stamp, true},
		{"stale override", "arm64", other, false},
		{"raw build", "arm64", "", false},
		{"duplicate identity", "arm64", stamp + stamp, false},
		{"wrong architecture", "amd64", stamp, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "executor")
			data := append(linuxELF(), tt.marker...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := artifactForBuild(path, tt.arch, stamp)
			if (err == nil) != tt.ok {
				t.Fatal(err)
			}
			if tt.ok && string(got) != string(data) {
				t.Fatal("did not return verified bytes")
			}
		})
	}
}

func TestArtifactRejectsUnversionedBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executor")
	if err := os.WriteFile(path, linuxELF(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Artifact(path, "arm64"); err == nil || !strings.Contains(err.Error(), "unversioned") {
		t.Fatalf("unversioned bundle accepted: %v", err)
	}
}
