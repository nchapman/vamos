package config

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestResourcesRoundTrip(t *testing.T) {
	for _, resources := range []string{"{cpus: 0.5}", "{memory_mb: 512}", "{cpus: 0.01, memory_mb: 6}", "{cpus: 1024, memory_mb: 1048576}"} {
		t.Run(resources, func(t *testing.T) {
			a, err := Decode(strings.NewReader("app: test\nservices:\n  worker:\n    image: alpine\n    resources: " + resources + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var round App
			if err := json.Unmarshal(b, &round); err != nil {
				t.Fatal(err)
			}
			again, _ := json.Marshal(round)
			if string(b) != string(again) {
				t.Fatalf("JSON changed: %s => %s", b, again)
			}
			y, err := yaml.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(strings.NewReader(string(y)))
			if err != nil {
				t.Fatal(err)
			}
			again, _ = json.Marshal(decoded)
			if string(b) != string(again) {
				t.Fatalf("YAML roundtrip changed: %s => %s", b, again)
			}
		})
	}
}

func TestResourcesInvalid(t *testing.T) {
	for _, resources := range []string{"{}", "{cpus: 0}", "{cpus: -1}", "{cpus: .nan}", "{cpus: .inf}", "{cpus: -.inf}", "{cpus: 0.0009}", "{cpus: 1024.001}", "{cpus: 0.1234}", "{cpus: 0.5000000000000001}", "{memory_mb: 0}", "{memory_mb: -1}", "{memory_mb: 5}", "{memory_mb: 1048577}", "{memory_mb: 6.5}", "{memory_mb: 9223372036854775808}", "{cpu: 1}"} {
		t.Run(resources, func(t *testing.T) {
			if _, err := Decode(strings.NewReader("app: test\nservices:\n  worker:\n    image: alpine\n    resources: " + resources + "\n")); err == nil {
				t.Fatal("accepted invalid resources")
			}
		})
	}
	for _, resources := range []string{`{}`, `{"CPUs":0}`, `{"MemoryMB":0}`, `{"CPUs":0.1234}`, `{"MemoryMB":5}`, `{"typo":1}`} {
		var a App
		err := json.Unmarshal([]byte(`{"Name":"test","Services":{"worker":{"Image":"alpine","Update":"auto","Resources":`+resources+`}}}`), &a)
		if err == nil {
			err = a.Validate()
		}
		if err == nil {
			t.Fatalf("accepted JSON resources %s", resources)
		}
	}
}

func TestResourcesExactConversionAndOptionalJSON(t *testing.T) {
	for milli := int64(10); milli <= 1024000; milli++ {
		cpu := float64(milli) / 1000
		r := &Resources{CPUs: &cpu}
		if err := r.Validate(); err != nil {
			t.Fatalf("valid milliCPU %d: %v", milli, err)
		}
		if r.NanoCPUs() != milli*1000000 {
			t.Fatalf("inexact conversion for %d", milli)
		}
	}
	cpu, memory := 0.5, int64(512)
	for _, test := range []struct {
		r    Resources
		want string
	}{
		{Resources{CPUs: &cpu}, `{"CPUs":0.5}`},
		{Resources{MemoryMB: &memory}, `{"MemoryMB":512}`},
	} {
		b, err := json.Marshal(test.r)
		if err != nil || string(b) != test.want {
			t.Fatalf("optional nested fields: %s, %v", b, err)
		}
	}
	for _, cpu := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		r := &Resources{CPUs: &cpu}
		if r.Validate() == nil {
			t.Fatal("nonfinite CPU accepted")
		}
	}
}

func TestResourcesNilCanonicalCompatibility(t *testing.T) {
	const old = `{"Build":null,"Image":"alpine","Command":null,"Env":null,"Secrets":null,"HTTP":null,"Update":"auto"}`
	var s Service
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil || string(b) != old {
		t.Fatalf("old canonical service changed: %s, %v", b, err)
	}
}
