package config

import (
	"errors"
	"fmt"
	"math"

	"go.yaml.in/yaml/v3"
)

// Resources are runtime hard caps, not build limits or reservations.
// MemoryMB is expressed in MiB (1024 * 1024 bytes).
type Resources struct {
	CPUs     *float64 `yaml:"cpus,omitempty" json:",omitempty"`
	MemoryMB *int64   `yaml:"memory_mb,omitempty" json:",omitempty"`
}

// YAML normally truncates floating-point values when decoding into integers.
// Decode memory separately so a fractional MiB request is never rounded down.
func (r *Resources) UnmarshalYAML(node *yaml.Node) error {
	var wire struct {
		CPUs     *float64 `yaml:"cpus"`
		MemoryMB any      `yaml:"memory_mb"`
	}
	if node.Kind != yaml.MappingNode {
		return errors.New("resources must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key != "cpus" && key != "memory_mb" {
			return fmt.Errorf("unknown resources field %q", key)
		}
	}
	if err := node.Decode(&wire); err != nil {
		return err
	}
	*r = Resources{CPUs: wire.CPUs}
	if wire.MemoryMB != nil {
		var n int64
		switch v := wire.MemoryMB.(type) {
		case int:
			n = int64(v)
		case int64:
			n = v
		case uint64:
			if v > math.MaxInt64 {
				return errors.New("memory_mb exceeds integer range")
			}
			n = int64(v)
		default:
			return errors.New("memory_mb must be an integer MiB count")
		}
		r.MemoryMB = &n
	}
	return nil
}

func (r *Resources) Validate() error {
	if r == nil {
		return nil
	}
	if r.CPUs == nil && r.MemoryMB == nil {
		return errors.New("must specify cpus or memory_mb")
	}
	if r.CPUs != nil {
		c := *r.CPUs
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0.01 || c > 1024 || math.Round(c*1000)/1000 != c {
			return errors.New("cpus must be between 0.01 and 1024 in increments of 0.001")
		}
	}
	if r.MemoryMB != nil && (*r.MemoryMB < 6 || *r.MemoryMB > 1048576) {
		return errors.New("memory_mb must be between 6 and 1048576 MiB")
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
	if r == nil || r.MemoryMB == nil {
		return 0
	}
	return *r.MemoryMB * 1024 * 1024
}
