package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/nchapman/vamos/internal/config"
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
// Every returned error is a fixed, value-free diagnostic safe for local CLI output.
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
	if !errors.Is(d.Decode(&extra), io.EOF) {
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
// Diagnostics are fixed, value-free messages safe for local CLI output. Never
// interpolate source labels, paths, field values or underlying parser errors.
func ValidatePlan(p Plan) error {
	if !ValidName(p.App) {
		return errors.New("app must be a lowercase name (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
	}
	if len(p.Sources) < 1 || len(p.Sources) > MaxSources {
		return errors.New("sources must contain 1–16 entries")
	}
	if len(p.Sets) < 1 || len(p.Sets) > MaxSets {
		return errors.New("sets must contain 1–8 entries")
	}
	// A plan with several errors should report the same first rule each time.
	for _, name := range slices.Sorted(maps.Keys(p.Sources)) {
		s := p.Sources[name]
		if !ValidName(name) || !ValidName(s.Service) {
			return errors.New("source labels and service references must be lowercase names (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
		}
		if err := validatePlanSource(s); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Sets)) {
		set := p.Sets[name]
		if !ValidName(name) || !ValidName(set.Repository) {
			return errors.New("set labels and repository references must be lowercase names (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
		}
		if err := validatePlanSet(set, p.Sources); err != nil {
			return err
		}
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxPlanBytes {
		return errors.New("backup plan exceeds the 16 KiB size limit")
	}
	return nil
}

func validatePlanSource(s Source) error {
	switch s.Type {
	case "postgres":
		if s.Volume != "" || s.Path != "" {
			return errors.New("PostgreSQL sources do not accept volume or path")
		}
		if !planIdentifier.MatchString(s.Database) || !planIdentifier.MatchString(s.User) {
			return errors.New("PostgreSQL database and user must be 1–63 ASCII letters, digits, underscores or hyphens")
		}
		if s.Port < 0 || s.Port > 65535 {
			return errors.New("PostgreSQL port must be 0–65535; 0 defaults to 5432")
		}
		if s.Credentials == nil {
			return errors.New("PostgreSQL sources require credentials.service and credentials.env")
		}
		if !ValidName(s.Credentials.Service) {
			return errors.New("credentials.service must be a lowercase service name (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
		}
		if !planEnv.MatchString(s.Credentials.Env) {
			return errors.New("credentials.env must be an environment-variable name (1–128 ASCII letters, digits or underscores; cannot start with a digit), not a secret name or value")
		}
		if s.Restore == nil || s.Restore.Profile != "single-owner" {
			return errors.New("PostgreSQL sources require restore.profile: single-owner")
		}
	case "sqlite", "files":
		if !ValidName(s.Volume) {
			return errors.New("SQLite/files sources require a lowercase volume name (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
		}
		if !validPlanPath(s.Path, s.Type == "files") {
			return errors.New("source path must be a clean relative path within the volume (at most 512 bytes and 32 components; no traversal, trailing slash, globs or expansion; '.' is files-only)")
		}
		if s.Database != "" || s.User != "" || s.Port != 0 || s.Credentials != nil || s.Restore != nil {
			return errors.New("SQLite/files sources do not accept database, user, port, credentials or restore")
		}
	default:
		return errors.New("source type must be postgres, sqlite or files")
	}
	return nil
}

func validatePlanSet(set Set, sources map[string]Source) error {
	if len(set.Sources) < 1 || len(set.Sources) > MaxSources {
		return errors.New("each set must select 1–16 sources")
	}
	if len(set.Stop) > config.MaxAppReplicas {
		return errors.New("stop list exceeds the app replica budget")
	}
	stopped := map[string]bool{}
	for _, svc := range set.Stop {
		if !ValidName(svc) || stopped[svc] {
			return errors.New("stop must contain unique lowercase service names (1–63 characters; start with a letter; use lowercase letters, digits or hyphens)")
		}
		stopped[svc] = true
	}
	switch set.Consistency {
	case "online-per-source":
		if len(set.Stop) != 0 {
			return errors.New("online-per-source sets must not declare stop services")
		}
	case "quiesced":
		if len(set.Stop) == 0 {
			return errors.New("quiesced sets require an explicit nonempty stop list")
		}
	default:
		return errors.New("consistency must be online-per-source or quiesced")
	}
	seen := map[string]bool{}
	pg := map[string]bool{}
	var selected []Source
	for _, key := range set.Sources {
		s, ok := sources[key]
		if !ok {
			return errors.New("set sources must reference declared source labels")
		}
		if seen[key] {
			return errors.New("set sources must not contain duplicate references")
		}
		seen[key] = true
		if s.Type == "postgres" {
			if stopped[s.Service] {
				return errors.New("selected PostgreSQL services must remain running; stop application writers instead")
			}
			if pg[s.Service] {
				return errors.New("a set may select only one PostgreSQL database per service")
			}
			pg[s.Service] = true
			continue
		}
		if s.Type == "files" && set.Consistency != "quiesced" {
			return errors.New("files sources require quiesced consistency")
		}
		if set.Consistency == "quiesced" && !stopped[s.Service] {
			return errors.New("a quiesced set must include each selected SQLite/files owner in stop")
		}
		for _, other := range selected {
			if sourcePathsOverlap(s, other) {
				return errors.New("selected paths overlap within a set (including SQLite WAL, shared-memory and rollback-journal paths)")
			}
		}
		selected = append(selected, s)
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
