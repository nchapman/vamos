// Package config loads and validates the app configuration described in PLAN.md.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

const DefaultFile = "soar.yaml"

type UpdatePolicy string

const (
	UpdateAuto   UpdatePolicy = "auto"
	UpdateManual UpdatePolicy = "manual"
)

type App struct {
	Name     string             `yaml:"app"`
	Services map[string]Service `yaml:"services"`
}

type Service struct {
	Build   *Build            `yaml:"build,omitempty"`
	Image   string            `yaml:"image,omitempty"`
	Command []string          `yaml:"command,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Secrets map[string]string `yaml:"secrets,omitempty"`
	HTTP    *HTTP             `yaml:"http,omitempty"`
	Volumes []Volume          `yaml:"volumes,omitempty"`
	Update  UpdatePolicy      `yaml:"update,omitempty"`
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
		if service.Update == "" {
			service.Update = UpdateAuto
			app.Services[name] = service
		}
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}
