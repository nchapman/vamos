package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	MaxPlanBytes = 16 * 1024
	MaxSources   = 16
	MaxSets      = 8
)

type Plan struct {
	App     string            `json:"app" yaml:"app"`
	Sources map[string]Source `json:"sources" yaml:"sources"`
	Sets    map[string]Set    `json:"sets" yaml:"sets"`
}
type Source struct {
	Type        string              `json:"type" yaml:"type"`
	Service     string              `json:"service" yaml:"service"`
	Volume      string              `json:"volume,omitempty" yaml:"volume,omitempty"`
	Path        string              `json:"path,omitempty" yaml:"path,omitempty"`
	Database    string              `json:"database,omitempty" yaml:"database,omitempty"`
	User        string              `json:"user,omitempty" yaml:"user,omitempty"`
	Port        int                 `json:"port,omitempty" yaml:"port,omitempty"`
	Credentials *CredentialSelector `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	Restore     *RestoreProfile     `json:"restore,omitempty" yaml:"restore,omitempty"`
}
type CredentialSelector struct {
	Service string `json:"service" yaml:"service"`
	Env     string `json:"env" yaml:"env"`
}
type RestoreProfile struct {
	Profile string `json:"profile" yaml:"profile"`
}
type Set struct {
	Sources     []string `json:"sources" yaml:"sources"`
	Consistency string   `json:"consistency" yaml:"consistency"`
	Stop        []string `json:"stop,omitempty" yaml:"stop,omitempty"`
	Repository  string   `json:"repository" yaml:"repository"`
}

// DecodePlan accepts exactly one bounded, strict YAML document. Parser diagnostics
// are deliberately not returned: malformed values may contain private material.
func DecodePlan(r io.Reader) (Plan, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxPlanBytes+1))
	if err != nil || len(b) > MaxPlanBytes {
		return Plan{}, errors.New("cannot read bounded backup plan")
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var n yaml.Node
	if d.Decode(&n) != nil || !safePlanNode(&n) {
		return Plan{}, errors.New("invalid backup plan YAML")
	}
	var extra yaml.Node
	if d.Decode(&extra) != io.EOF {
		return Plan{}, errors.New("backup plan requires one document")
	}
	d = yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	var p Plan
	if d.Decode(&p) != nil {
		return Plan{}, errors.New("invalid backup plan fields")
	}
	return NormalizePlan(p)
}

func safePlanNode(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode || n.Tag == "!!null" || n.Tag == "!!merge" || n.Style&yaml.TaggedStyle != 0 {
		return false
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return false
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" || seen[k.Value] {
				return false
			}
			seen[k.Value] = true
		}
	case yaml.SequenceNode:
	case yaml.ScalarNode:
		if n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!bool" && n.Tag != "!!float" {
			return false
		}
	default:
		return false
	}
	for _, c := range n.Content {
		if !safePlanNode(c) {
			return false
		}
	}
	return true
}

var planIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
var planEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func validPlanPath(s string, root bool) bool {
	if s == "." {
		return root
	}
	if s == "" || len(s) > 512 || path.IsAbs(s) || path.Clean(s) != s || strings.ContainsAny(s, `\*?[]${}`) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	parts := strings.Split(s, "/")
	if len(parts) > 32 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}

// ValidatePlan checks selection policy, not runtime capabilities or capture proof.
// In particular, a stop list cannot prove that every PostgreSQL writer is stopped.
func ValidatePlan(p Plan) error {
	bad := func() error { return errors.New("invalid backup plan selection") }
	if !ValidName(p.App) || len(p.Sources) < 1 || len(p.Sources) > MaxSources || len(p.Sets) < 1 || len(p.Sets) > MaxSets {
		return bad()
	}
	for name, s := range p.Sources {
		if !ValidName(name) || !ValidName(s.Service) {
			return bad()
		}
		switch s.Type {
		case "postgres":
			if s.Volume != "" || s.Path != "" || !planIdentifier.MatchString(s.Database) || !planIdentifier.MatchString(s.User) || s.Port < 0 || s.Port > 65535 || s.Credentials == nil || !ValidName(s.Credentials.Service) || !planEnv.MatchString(s.Credentials.Env) || s.Restore == nil || s.Restore.Profile != "single-owner" {
				return bad()
			}
		case "sqlite", "files":
			if !ValidName(s.Volume) || !validPlanPath(s.Path, s.Type == "files") || s.Database != "" || s.User != "" || s.Port != 0 || s.Credentials != nil || s.Restore != nil {
				return bad()
			}
		default:
			return bad()
		}
	}
	for name, set := range p.Sets {
		if !ValidName(name) || !ValidName(set.Repository) || len(set.Sources) < 1 || len(set.Sources) > MaxSources || len(set.Stop) > 8 {
			return bad()
		}
		stopped := map[string]bool{}
		for _, svc := range set.Stop {
			if !ValidName(svc) || stopped[svc] {
				return bad()
			}
			stopped[svc] = true
		}
		switch set.Consistency {
		case "online-per-source":
			if len(set.Stop) != 0 {
				return bad()
			}
		case "quiesced":
			if len(set.Stop) == 0 {
				return bad()
			}
		default:
			return bad()
		}
		seen := map[string]bool{}
		pg := map[string]bool{}
		var selected []Source
		for _, key := range set.Sources {
			s, ok := p.Sources[key]
			if !ok || seen[key] {
				return bad()
			}
			seen[key] = true
			if s.Type == "postgres" {
				if stopped[s.Service] || pg[s.Service] {
					return bad()
				}
				pg[s.Service] = true
			} else {
				if set.Consistency == "quiesced" && !stopped[s.Service] || s.Type == "files" && set.Consistency != "quiesced" {
					return bad()
				}
				for _, other := range selected {
					if sourcePathsOverlap(s, other) {
						return bad()
					}
				}
				selected = append(selected, s)
			}
		}
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxPlanBytes {
		return errors.New("backup plan exceeds size limit")
	}
	return nil
}

func sourcePathsOverlap(a, b Source) bool {
	if a.Service != b.Service || a.Volume != b.Volume {
		return false
	}
	paths := func(s Source) []string {
		if s.Type == "sqlite" {
			return []string{s.Path, s.Path + "-wal", s.Path + "-shm", s.Path + "-journal"}
		}
		return []string{s.Path}
	}
	for _, x := range paths(a) {
		for _, y := range paths(b) {
			if x == "." || y == "." || x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}

// NormalizePlan returns an independent, canonical selection without mutating p.
func NormalizePlan(p Plan) (Plan, error) {
	if err := ValidatePlan(p); err != nil {
		return Plan{}, err
	}
	out := Plan{App: p.App, Sources: make(map[string]Source, len(p.Sources)), Sets: make(map[string]Set, len(p.Sets))}
	for k, s := range p.Sources {
		if s.Type == "postgres" && s.Port == 0 {
			s.Port = 5432
		}
		if s.Credentials != nil {
			c := *s.Credentials
			s.Credentials = &c
		}
		if s.Restore != nil {
			r := *s.Restore
			s.Restore = &r
		}
		out.Sources[k] = s
	}
	for k, s := range p.Sets {
		s.Sources = append([]string(nil), s.Sources...)
		s.Stop = append([]string(nil), s.Stop...)
		sort.Strings(s.Sources)
		sort.Strings(s.Stop)
		out.Sets[k] = s
	}
	if err := ValidatePlan(out); err != nil {
		return Plan{}, err
	}
	return out, nil
}

// PlanDigest identifies the normalized policy, not a captured recovery point.
func PlanDigest(p Plan) (string, error) {
	n, err := NormalizePlan(p)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return "", errors.New("cannot encode backup plan")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
