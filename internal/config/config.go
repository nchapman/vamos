// Package config loads and validates the app configuration described in PLAN.md.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

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

const (
	MaxAppReplicas    = 32
	MaxSecretMappings = 64
)

// ReplicaCount returns the effective per-service count; omission is one.
func (s Service) ReplicaCount() int {
	if s.Replicas == nil {
		return 1
	}
	return *s.Replicas
}

// NormalizeReplicas canonicalizes default replicas, network mode and HTTP features for
// captured configuration equality and canonical historical run records.
func (s *Service) NormalizeReplicas() {
	if s.NetworkMode == "bridge" {
		s.NetworkMode = ""
	}
	if s.Replicas != nil && *s.Replicas == 1 {
		s.Replicas = nil
	}
	if s.HTTP != nil {
		s.HTTP.NormalizeFeatures()
	}
}

type Service struct {
	NetworkMode string            `yaml:"network_mode,omitempty" json:"NetworkMode,omitempty"`
	Jobs        map[string]Job    `yaml:"jobs,omitempty" json:",omitempty"`
	Ports       []string          `yaml:"ports,omitempty" json:"Ports,omitempty"`
	Replicas    *int              `yaml:"replicas,omitempty" json:"Replicas,omitempty"`
	Build       *Build            `yaml:"build,omitempty"`
	Image       string            `yaml:"image,omitempty"`
	Command     []string          `yaml:"command,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Secrets     map[string]string `yaml:"secrets,omitempty"`
	HTTP        *HTTP             `yaml:"http,omitempty"`
	Volume      *Volume           `yaml:"volume,omitempty" json:"Volume,omitempty"`
	Update      UpdatePolicy      `yaml:"update,omitempty"`
	Resources   *Resources        `yaml:"resources,omitempty" json:",omitempty"`
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

func (b *Build) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.ShortTag() == "!!str" {
		*b = Build{Context: value.Value, Dockerfile: "Dockerfile"}
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: build must be a context path string or a {context, dockerfile} mapping", value.Line)
	}
	if err := checkMappingKeys(value, "config.Build", "context", "dockerfile"); err != nil {
		return err
	}
	var raw struct {
		Context    *string `yaml:"context"`
		Dockerfile *string `yaml:"dockerfile"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	b.Context = "."
	if raw.Context != nil {
		b.Context = *raw.Context
	}
	b.Dockerfile = "Dockerfile"
	if raw.Dockerfile != nil {
		b.Dockerfile = *raw.Dockerfile
	}
	return nil
}

type HTTP struct {
	Port        int        `yaml:"port"`
	Health      string     `yaml:"health"`
	Domains     []string   `yaml:"domains,omitempty"`
	WAF         *bool      `yaml:"waf,omitempty" json:"WAF,omitempty"`
	Cache       *bool      `yaml:"cache,omitempty" json:"Cache,omitempty"`
	Compression *bool      `yaml:"compression,omitempty" json:"Compression,omitempty"`
	RateLimit   []RateRule `yaml:"rate_limit,omitempty" json:"RateLimit,omitempty"`
	Redirects   []Redirect `yaml:"redirects,omitempty" json:"Redirects,omitempty"`
}

// WAFEnabled reports whether the OWASP Coraza WAF is enabled; omission is true.
func (h HTTP) WAFEnabled() bool {
	if h.WAF == nil {
		return true
	}
	return *h.WAF
}

// CacheEnabled reports whether the HTTP cache is enabled; omission is true.
func (h HTTP) CacheEnabled() bool {
	return h.Cache == nil || *h.Cache
}

// CompressionEnabled reports whether Zstandard/Gzip encoding is enabled; omission is true.
func (h HTTP) CompressionEnabled() bool {
	return h.Compression == nil || *h.Compression
}

// NormalizeFeatures keeps explicit true equivalent to omission.
func (h *HTTP) NormalizeFeatures() {
	if h == nil {
		return
	}
	for _, feature := range []**bool{&h.WAF, &h.Cache, &h.Compression} {
		if *feature != nil && **feature {
			*feature = nil
		}
	}
}

func (h *HTTP) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: http must be a mapping", value.Line)
	}
	if err := checkMappingKeys(value, "config.HTTP", "port", "health", "domains", "waf", "cache", "compression", "rate_limit", "redirects"); err != nil {
		return err
	}
	var raw struct {
		Port        int        `yaml:"port"`
		Health      *string    `yaml:"health"`
		Domains     []string   `yaml:"domains,omitempty"`
		WAF         *bool      `yaml:"waf,omitempty"`
		Cache       *bool      `yaml:"cache,omitempty"`
		Compression *bool      `yaml:"compression,omitempty"`
		RateLimit   []RateRule `yaml:"rate_limit,omitempty"`
		Redirects   []Redirect `yaml:"redirects,omitempty"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	h.Port = raw.Port
	h.Health = "/"
	if raw.Health != nil {
		h.Health = *raw.Health
	}
	h.Domains = raw.Domains
	h.WAF = raw.WAF
	h.Cache = raw.Cache
	h.Compression = raw.Compression
	h.RateLimit = raw.RateLimit
	h.Redirects = raw.Redirects
	h.NormalizeFeatures()
	return nil
}

type Volume struct {
	Name  string `yaml:"name"`
	Mount string `yaml:"mount"`
}

func (v *Volume) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.ShortTag() == "!!str" {
		name, mount, ok := strings.Cut(value.Value, ":")
		if !ok {
			return fmt.Errorf("line %d: volume shorthand must be NAME:/container/path", value.Line)
		}
		*v = Volume{Name: name, Mount: mount}
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: volume must be NAME:/container/path or a {name, mount} mapping", value.Line)
	}
	if err := checkMappingKeys(value, "config.Volume", "name", "mount"); err != nil {
		return err
	}
	type volumeYAML Volume
	var raw volumeYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*v = Volume(raw)
	return nil
}

func checkMappingKeys(node *yaml.Node, typeName string, allowed ...string) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if !slices.Contains(allowed, key.Value) {
			return fmt.Errorf("line %d: field %s not found in type %s", key.Line, key.Value, typeName)
		}
	}
	return nil
}

const maxConfigBytes = 1 << 20

// Load reads a local file. It does not contact a server or inspect build contexts.
func Load(filename string) (*App, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: configuration must be a regular file", filename)
	}
	if info.Size() > maxConfigBytes {
		return nil, fmt.Errorf("%s: configuration exceeds %d bytes", filename, maxConfigBytes)
	}

	app, err := Decode(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return app, nil
}

// Decode accepts exactly one YAML document and rejects unknown fields.
func Decode(r io.Reader) (*App, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("configuration exceeds %d bytes", maxConfigBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var app App
	if err := decoder.Decode(&app); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration is empty")
		}
		if hint := commandListHint(data); hint != "" {
			return nil, fmt.Errorf("decode YAML: %s", hint)
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

func commandListHint(data []byte) string {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return ""
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value == "release" && v.Kind == yaml.MappingNode {
			if cmd := lookupMappingValue(v, "command"); cmd != nil && cmd.Kind == yaml.ScalarNode && cmd.ShortTag() == "!!str" {
				return fmt.Sprintf("line %d: release.command must be a YAML list of arguments (e.g. [\"bin/release\"]), not a shell string", cmd.Line)
			}
		}
		if k.Value == "services" && v.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(v.Content); j += 2 {
				svcName, svcNode := v.Content[j], v.Content[j+1]
				if cmd := lookupMappingValue(svcNode, "command"); cmd != nil && cmd.Kind == yaml.ScalarNode && cmd.ShortTag() == "!!str" {
					return fmt.Sprintf("line %d: services.%s.command must be a YAML list of arguments (e.g. [\"bundle\", \"exec\", \"puma\"]), not a shell string", cmd.Line, svcName.Value)
				}
			}
		}
	}
	return ""
}

func lookupMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
