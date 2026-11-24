package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNetworkModeSchema(t *testing.T) {
	base := "app: notes\nservices:\n  worker:\n    image: demo\n"
	decode := func(fields string) *App {
		t.Helper()
		a, err := Decode(strings.NewReader(base + fields))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	defaultApp := decode("")
	if !reflect.DeepEqual(defaultApp, decode("    network_mode: bridge\n")) {
		t.Fatal("explicit bridge differs from omission")
	}
	host := "    network_mode: host\n"
	if got := decode(host + "    replicas: 1\n").Services["worker"].NetworkMode; got != "host" {
		t.Fatalf("mode %q", got)
	}
	for _, fields := range []string{"    network_mode: container:other\n", host + "    http: {port: 8080}\n", host + "    ports: ['8080:8080']\n", host + "    replicas: 2\n"} {
		if _, err := Decode(strings.NewReader(base + fields)); err == nil || !strings.Contains(err.Error(), "services.worker.network_mode") {
			t.Fatalf("%q: %v", fields, err)
		}
	}
}

func TestNetworkModeWire(t *testing.T) {
	for _, tc := range []struct{ input, mode string }{{`{"Volumes":null}`, ""}, {`{"NetworkMode":"bridge"}`, ""}, {`{"NetworkMode":"host"}`, "host"}} {
		var s Service
		if err := json.Unmarshal([]byte(tc.input), &s); err != nil || s.NetworkMode != tc.mode {
			t.Fatalf("decode %s: mode %q, %v", tc.input, s.NetworkMode, err)
		}
		b, err := json.Marshal(s)
		if err != nil || bytes.Contains(b, []byte(`"NetworkMode"`)) != (tc.mode != "") {
			t.Fatalf("encode: %s, %v", b, err)
		}
	}
}
