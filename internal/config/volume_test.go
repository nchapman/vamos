package config_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestSingularVolumeConfiguration(t *testing.T) {
	for _, field := range []string{"volume: {name: data, mount: /data}", "volume:\n      name: data\n      mount: /data"} {
		a, err := config.Decode(strings.NewReader("app: notes\nservices:\n  web:\n    image: app:v1\n    " + field + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(a.Services["web"])
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		var volume config.Volume
		if err := json.Unmarshal(wire["Volume"], &volume); err != nil {
			t.Fatal(err)
		}
		if volume.Name != "data" || volume.Mount != "/data" {
			t.Fatalf("volume lost: %+v", volume)
		}
		var restored config.Service
		if err := json.Unmarshal(b, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Volume == nil || *restored.Volume != volume {
			t.Fatalf("service JSON round trip lost volume: %+v", restored)
		}
		if _, exists := wire["Volumes"]; exists {
			t.Fatal("new service JSON still emits plural volumes")
		}
	}
}

func TestPluralAndMalformedVolumeConfigurationIsRejected(t *testing.T) {
	for _, field := range []string{
		"volumes: [{name: data, mount: /data}]",
		"volumes: []",
		"volumes: null",
		"volume: {name: data, mount: /data}\n    volumes: []",
		"volume: [{name: data, mount: /data}]",
		"volume: {name: data, mount: /data, typo: true}",
		"volume: {}",
		"volume: {name: data}",
		"volume: {mount: /data}",
	} {
		t.Run(field, func(t *testing.T) {
			if _, err := config.Decode(strings.NewReader("app: notes\nservices:\n  web:\n    image: app:v1\n    " + field + "\n")); err == nil {
				t.Fatal("unsupported volume syntax accepted")
			}
		})
	}
}

func TestLegacyStatelessServiceJSONCompatibility(t *testing.T) {
	// Existing request and v1/v2 journal services serialized Volumes even when
	// stateless. Accept only those historical empty values, never real mounts.
	for _, value := range []string{"null", "[]"} {
		t.Run(value, func(t *testing.T) {
			var s config.Service
			d := json.NewDecoder(strings.NewReader(`{"Build":{"Context":".","Dockerfile":"Dockerfile"},"Update":"auto","Volumes":` + value + `}`))
			d.DisallowUnknownFields()
			if err := d.Decode(&s); err != nil {
				t.Fatal("existing stateless service no longer readable:", err)
			}
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(`"Volumes"`)) || bytes.Contains(b, []byte(`"Volume"`)) {
				t.Fatal("empty legacy storage field was retained", string(b))
			}
		})
	}
}

func TestServiceJSONRejectsLegacyMountsAndUnknownFields(t *testing.T) {
	for _, input := range []string{
		`{"Volumes":[{"Name":"data","Mount":"/data"}]}`,
		`{"Volumes":{}}`,
		`{"Volume":[]}`,
		`{"Volumes":"data"}`,
		`{"Volume":{"Name":"data","Mount":"/data"},"Volumes":[{"Name":"other","Mount":"/other"}]}`,
		`{"Volume":{"Name":"data","Mount":"/data","Typo":true}}`,
		`{"Build":{"Context":".","Dockerfile":"Dockerfile","Typo":true}}`,
		`{"Typo":true}`,
	} {
		t.Run(input, func(t *testing.T) {
			var s config.Service
			d := json.NewDecoder(strings.NewReader(input))
			d.DisallowUnknownFields()
			if err := d.Decode(&s); err == nil {
				t.Fatal("unsafe or unknown service JSON accepted")
			}
		})
	}
}
