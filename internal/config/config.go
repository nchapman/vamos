// Package config loads and validates the app configuration described in PLAN.md.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

const DefaultFile = "vamos.yaml"

type UpdatePolicy string

const (
	UpdateAuto   UpdatePolicy = "auto"
	UpdateManual UpdatePolicy = "manual"
)

type App struct {
	Name     string             `yaml:"app"`
	Services map[string]Service `yaml:"services"`
	Release  *Release           `yaml:"release,omitempty" json:",omitempty"`
}

// Release is an operation-scoped command, not persistent remote app policy.
// It uses one selected service's newly resolved image and captured environment.
type Release struct {
	Service string   `yaml:"service"`
	Command []string `yaml:"command"`
}

const MaxReplicas = 8
const MaxAppReplicas = 32

// ReplicaCount returns the effective per-service count; omission is one.
func (s Service) ReplicaCount() int {
	if s.Replicas == nil {
		return 1
	}
	return *s.Replicas
}

// NormalizeReplicas keeps explicit one equivalent to omission, including in
// captured configuration equality and canonical historical run records.
func (s *Service) NormalizeReplicas() {
	if s.Replicas != nil && *s.Replicas == 1 {
		s.Replicas = nil
	}
}

type Service struct {
	Replicas  *int              `yaml:"replicas,omitempty" json:"Replicas,omitempty"`
	Build     *Build            `yaml:"build,omitempty"`
	Image     string            `yaml:"image,omitempty"`
	Command   []string          `yaml:"command,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	Secrets   map[string]string `yaml:"secrets,omitempty"`
	HTTP      *HTTP             `yaml:"http,omitempty"`
	Volume    *Volume           `yaml:"volume,omitempty" json:"Volume,omitempty"`
	Update    UpdatePolicy      `yaml:"update,omitempty"`
	Resources *Resources        `yaml:"resources,omitempty" json:",omitempty"`
}

// UnmarshalJSON preserves stateless requests and journals written before the
// singular volume schema. Their Volumes field was null or empty, never deployed
// storage. Keep strict nested decoding and refuse nonempty legacy mounts rather
// than silently discarding storage configuration.
func (s *Service) UnmarshalJSON(data []byte) error {
	type serviceJSON Service
	var decoded serviceJSON
	wire := struct {
		*serviceJSON
		LegacyVolumes json.RawMessage `json:"Volumes"`
	}{serviceJSON: &decoded}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if len(wire.LegacyVolumes) != 0 {
		var legacy []json.RawMessage
		if err := json.Unmarshal(wire.LegacyVolumes, &legacy); err != nil || len(legacy) != 0 {
			return errors.New("legacy JSON Volumes must be null or empty; use singular Volume")
		}
	}
	*s = Service(decoded)
	s.NormalizeReplicas()
	return nil
}

type Build struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
}

type HTTP struct {
	Port    int      `yaml:"port"`
	Health  string   `yaml:"health"`
	Domains []string `yaml:"domains,omitempty"`
}

type Volume struct {
	Name  string `yaml:"name"`
	Mount string `yaml:"mount"`
}

// Load reads a local file. It does not contact a server or inspect build contexts.
func Load(filename string) (*App, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	app, err := Decode(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return app, nil
}

// Decode accepts exactly one YAML document and rejects unknown fields.
func Decode(r io.Reader) (*App, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var app App
	if err := decoder.Decode(&app); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration is empty")
		}
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode trailing YAML: %w", err)
		}
		return nil, errors.New("expected exactly one YAML document")
	}

	for name, service := range app.Services {
		service.NormalizeReplicas()
		if service.Update == "" {
			service.Update = UpdateAuto
		}
		app.Services[name] = service
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}
