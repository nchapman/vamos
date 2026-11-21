package config

import (
	"errors"
	"fmt"
	"math"

	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"
)

// Resources are runtime hard caps, not build limits or reservations.
// Memory is normalized to bytes before comparison, capture, or execution.
type Resources struct {
	CPUs   *float64 `yaml:"cpus,omitempty" json:",omitempty"`
	Memory *int64   `yaml:"memory,omitempty" json:",omitempty"`
}

// Parse memory at the manifest boundary using Docker's --memory grammar.
func (r *Resources) UnmarshalYAML(node *yaml.Node) error {
	var wire struct {
		CPUs   *float64  `yaml:"cpus"`
		Memory yaml.Node `yaml:"memory"`
	}
	if node.Kind != yaml.MappingNode {
		return errors.New("resources must be a mapping")
	}
	if err := checkMappingKeys(node, "config.Resources", "cpus", "memory"); err != nil {
		return err
	}
	if err := node.Decode(&wire); err != nil {
		return err
	}
	*r = Resources{CPUs: wire.CPUs}
	if wire.Memory.Kind != 0 {
		var size string
		if err := wire.Memory.Decode(&size); err != nil {
			return fmt.Errorf("memory must be a Docker size (for example 512m): %w", err)
		}
		n, err := units.RAMInBytes(size)
		if err != nil {
			return fmt.Errorf("invalid memory size: %w", err)
		}
		r.Memory = &n
	}
	return nil
}

func (r *Resources) Validate() error {
	if r == nil {
		return nil
	}
	if r.CPUs == nil && r.Memory == nil {
		return errors.New("must specify cpus or memory")
	}
	if r.CPUs != nil {
		c := *r.CPUs
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0.01 || c > 1024 || math.Round(c*1000)/1000 != c {
			return errors.New("cpus must be between 0.01 and 1024 in increments of 0.001")
		}
	}
	if r.Memory != nil && (*r.Memory < 6<<20 || *r.Memory > 1<<40) {
		return errors.New("memory must be between 6 MiB and 1 TiB (bare numbers are bytes)")
	}
	return nil
}

// NanoCPUs converts validated milli-CPU precision using integer multiplication,
// avoiding truncation of a floating point CPU-to-nanosecond product.
func (r *Resources) NanoCPUs() int64 {
	if r == nil || r.CPUs == nil {
		return 0
	}
	return int64(math.Round(*r.CPUs*1000)) * 1000000
}

func (r *Resources) MemoryBytes() int64 {
	if r == nil || r.Memory == nil {
		return 0
	}
	return *r.Memory
}
