package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestDecodeApp(t *testing.T) {
	app, err := config.Decode(strings.NewReader(`
app: notes
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    command: ["./server", ""]
    env:
      MODE: production
    secrets:
      SESSION_SECRET: session-secret
    http:
      port: 8080
      health: /ready?check=db
      domains: [notes.example.com]
    volume:
      name: data
      mount: /data
  db:
    image: postgres:17
    update: manual
`))
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "notes" || len(app.Services) != 2 {
		t.Fatalf("unexpected app: %+v", app)
	}
	web := app.Services["web"]
	if web.Update != config.UpdateAuto {
		t.Fatalf("volume-backed app should default to auto, got %q", web.Update)
	}
	if app.Services["db"].Update != config.UpdateManual {
		t.Fatal("manual update policy was not preserved")
	}
	if web.Build.Context != "." || web.HTTP.Port != 8080 || web.Secrets["SESSION_SECRET"] != "session-secret" || web.Volume == nil || web.Volume.Mount != "/data" {
		t.Fatalf("configuration was not preserved: %+v", web)
	}
	if len(web.Command) != 2 || web.Command[1] != "" {
		t.Fatalf("command argument boundaries were not preserved: %#v", web.Command)
	}
}

func TestDecodeShorthandsAndDefaults(t *testing.T) {
	app, err := config.Decode(strings.NewReader(`
app: notes
services:
  web:
    build: .
    http:
      port: 8080
    volume: data:/data
  api:
    build:
      context: ./api
    http:
      port: 8081
      waf: false
  worker:
    build:
      dockerfile: Dockerfile.worker
    http:
      port: 8082
      waf: true
`))
	if err != nil {
		t.Fatal(err)
	}
	web := app.Services["web"]
	if web.Build == nil || web.Build.Context != "." || web.Build.Dockerfile != "Dockerfile" {
		t.Fatalf("unexpected web build: %+v", web.Build)
	}
	if web.HTTP == nil || web.HTTP.Port != 8080 || web.HTTP.Health != "/" || !web.HTTP.WAFEnabled() || web.HTTP.WAF != nil {
		t.Fatalf("unexpected web http: %+v", web.HTTP)
	}
	if web.Volume == nil || web.Volume.Name != "data" || web.Volume.Mount != "/data" {
		t.Fatalf("unexpected web volume: %+v", web.Volume)
	}
	api := app.Services["api"]
	if api.Build == nil || api.Build.Context != "./api" || api.Build.Dockerfile != "Dockerfile" {
		t.Fatalf("unexpected api build: %+v", api.Build)
	}
	if api.HTTP == nil || api.HTTP.WAFEnabled() || api.HTTP.WAF == nil || *api.HTTP.WAF {
		t.Fatalf("expected api WAF disabled: %+v", api.HTTP)
	}
	worker := app.Services["worker"]
	if worker.Build == nil || worker.Build.Context != "." || worker.Build.Dockerfile != "Dockerfile.worker" {
		t.Fatalf("unexpected worker build: %+v", worker.Build)
	}
	if worker.HTTP == nil || !worker.HTTP.WAFEnabled() || worker.HTTP.WAF != nil {
		t.Fatalf("expected explicit waf: true to normalize to nil: %+v", worker.HTTP)
	}
}

func TestDecodeRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", "empty"},
		{"null", "null", "app must"},
		{"malformed", "app: [", "decode YAML"},
		{"unknown top-level field", "app: notes\nservcies: {}", "servcies"},
		{"multiple documents", "app: notes\nservices: {}\n---\napp: other", "exactly one"},
		{"trailing empty document", "app: notes\nservices: {}\n---\n", "exactly one"},
		{"trailing malformed document", "app: notes\nservices: {}\n---\n[", "trailing YAML"},
		{"duplicate keys", "app: notes\napp: other", "already defined"},
		{"no services", "app: notes\nservices: {}", "at least one"},
		{"invalid app name", "app: Bad_Name\nservices: {}", "app must"},
		{"long app name", "app: " + strings.Repeat("a", 64), "app must"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertInvalid(t, tt.input, tt.want)
		})
	}
}

func TestDecodeRejectsInvalidServices(t *testing.T) {
	tests := []struct {
		name, fields, want string
	}{
		{"no source", "env: {}", "exactly one"},
		{"both sources", "image: app:v1\n    build: {context: ., dockerfile: Dockerfile}", "exactly one"},
		{"unknown field", "image: app:v1\n    lifecycle: persistent", "lifecycle"},
		{"unknown nested field", "build: {context: ., dockerfile: Dockerfile, typo: true}", "typo"},
		{"empty build context", "build: {context: '', dockerfile: Dockerfile}", "build.context"},
		{"empty build string", "build: ''", "build.context"},
		{"absolute build context", "build: {context: /tmp/app, dockerfile: Dockerfile}", "build.context"},
		{"empty Dockerfile", "build: {context: ., dockerfile: ''}", "build.dockerfile"},
		{"escaping Dockerfile", "build: {context: ., dockerfile: ../Dockerfile}", "build.dockerfile"},
		{"absolute Dockerfile", "build: {context: ., dockerfile: /Dockerfile}", "build.dockerfile"},
		{"invalid image whitespace", "image: ' app:v1'", "image must"},
		{"invalid image reference", "image: UPPERCASE:v1", "invalid image reference"},
		{"invalid update policy", "image: app:v1\n    update: automatic", "update must"},
		{"string command", "image: app:v1\n    command: echo hi", "must be a YAML list of arguments"},
		{"empty command", "image: app:v1\n    command: []", "nonempty executable"},
		{"empty executable", "image: app:v1\n    command: ['', argument]", "nonempty executable"},
		{"NUL argument", "image: app:v1\n    command: [\"app\\0\"]", "command must not contain NUL"},
		{"invalid env name", "image: app:v1\n    env: {BAD-NAME: value}", "environment variable name"},
		{"NUL env value", "image: app:v1\n    env: {TOKEN: \"value\\0\"}", "must not contain NUL"},
		{"invalid secret name", "image: app:v1\n    secrets: {TOKEN: BAD_NAME}", "bad-name"},
		{"invalid secret env name", "image: app:v1\n    secrets: {BAD-NAME: token}", "environment variable name"},
		{"env secret collision", "image: app:v1\n    env: {TOKEN: literal}\n    secrets: {TOKEN: token}", "both env and secrets"},
		{"zero HTTP port", "image: app:v1\n    http: {port: 0, health: /ready}", "http.port"},
		{"large HTTP port", "image: app:v1\n    http: {port: 65536, health: /ready}", "http.port"},
		{"empty health path", "image: app:v1\n    http: {port: 80, health: ''}", "http.health"},
		{"absolute health URL", "image: app:v1\n    http: {port: 80, health: 'https://example.com/'}", "http.health"},
		{"network health URL", "image: app:v1\n    http: {port: 80, health: '//example.com/'}", "http.health"},
		{"health fragment", "image: app:v1\n    http: {port: 80, health: '/ready#fragment'}", "http.health"},
		{"relative volume", "image: app:v1\n    volume: {name: data, mount: data}", "absolute container directory"},
		{"relative volume shorthand", "image: app:v1\n    volume: data:data", "absolute container directory"},
		{"missing colon volume shorthand", "image: app:v1\n    volume: data", "NAME:/container/path"},
		{"root volume", "image: app:v1\n    volume: {name: data, mount: /}", "absolute container directory"},
		{"unclean volume", "image: app:v1\n    volume: {name: data, mount: /data/../other}", "absolute container directory"},
		{"invalid volume name", "image: app:v1\n    volume: {name: /host/path, mount: /data}", "volume name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertInvalid(t, "app: notes\nservices:\n  web:\n    "+tt.fields+"\n", tt.want)
		})
	}
}

func TestDecodeRejectsInvalidDomains(t *testing.T) {
	for _, domain := range []string{"https://notes.example.com", "*.example.com", "localhost", "notes.example.com:443", "example.com/path", "example.com.", "127.0.0.1", "bad_name.example.com"} {
		t.Run(domain, func(t *testing.T) {
			input := "app: notes\nservices:\n  web:\n    image: app:v1\n    http:\n      port: 80\n      health: /\n      domains: ['" + domain + "']\n"
			assertInvalid(t, input, "must be a DNS hostname")
		})
	}
}

func TestDecodeRejectsCrossServiceConflicts(t *testing.T) {
	t.Run("shared volume", func(t *testing.T) {
		assertInvalid(t, `app: notes
services:
  web:
    image: app:v1
    volume: {name: data, mount: /data}
  worker:
    image: app:v1
    volume: {name: data, mount: /other}
`, "shared volumes are not supported")
	})
	t.Run("case-insensitive domain collision", func(t *testing.T) {
		assertInvalid(t, `app: notes
services:
  web:
    image: app:v1
    http: {port: 80, health: /, domains: [Notes.example.com]}
  api:
    image: app:v1
    http: {port: 80, health: /, domains: [notes.example.com]}
`, "already assigned")
	})
	t.Run("invalid service name", func(t *testing.T) {
		assertInvalid(t, "app: notes\nservices:\n  Bad_Name:\n    image: app:v1", "service name")
	})
}

func TestValidationErrorsAreStable(t *testing.T) {
	input := "app: notes\nservices:\n  z: {update: wrong}\n  a: {update: wrong}\n"
	_, first := config.Decode(strings.NewReader(input))
	if first == nil {
		t.Fatal("expected validation errors")
	}
	for range 20 {
		_, err := config.Decode(strings.NewReader(input))
		if err == nil || err.Error() != first.Error() {
			t.Fatalf("unstable diagnostic: %v; want %v", err, first)
		}
	}
}

func TestLoadExamples(t *testing.T) {
	files, err := filepath.Glob("../../examples/*/vamos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("found %d examples, want 8", len(files))
	}
	for _, filename := range files {
		t.Run(filepath.Base(filepath.Dir(filename)), func(t *testing.T) {
			if _, err := config.Load(filename); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want wrapped not-exist error", err)
	}
}

func assertInvalid(t *testing.T, input, want string) {
	t.Helper()
	app, err := config.Decode(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got app=%+v err=%v; want error containing %q", app, err, want)
	}
	if app != nil {
		t.Fatal("invalid input returned a partial app")
	}
}
