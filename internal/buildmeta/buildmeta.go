// Package buildmeta identifies a client/executor/agent bundle, independently of
// wire protocols and persisted state versions. It is a pairing guard, not a signature.
package buildmeta

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
)

const (
	prefix = "soar-bundle-v1:"
	suffix = ":end-soar-bundle"
)

// Stamp is injected at link time by cmd/build. Referencing it through Version
// and Current retains the marker even in stripped binaries.
var Stamp string

var (
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)
	idPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	stampPattern = regexp.MustCompile(prefix + `([a-f0-9]{64}):([A-Za-z0-9][A-Za-z0-9.+_-]{0,63})` + suffix)
)

type Info struct {
	Label string `json:"label"`
	ID    string `json:"id"`
	Stamp string `json:"stamp,omitempty"`
}

func ValidLabel(label string) bool { return labelPattern.MatchString(label) }
func ValidID(id string) bool       { return idPattern.MatchString(id) }

func Encode(id, label string) (string, error) {
	stamp := prefix + id + ":" + label + suffix
	if _, err := Parse(stamp); err != nil {
		return "", err
	}
	return stamp, nil
}

func Parse(stamp string) (Info, error) {
	match := stampPattern.FindStringSubmatch(stamp)
	if match == nil || match[0] != stamp {
		return Info{}, errors.New("invalid or unversioned bundle identity; run mise run build and keep the whole bundle together")
	}
	return Info{ID: match[1], Label: match[2], Stamp: stamp}, nil
}

func Current() (Info, error) {
	return Parse(Stamp)
}

func Version() string {
	info, err := Current()
	if err != nil {
		return "dev (unversioned; run mise run build)"
	}
	return fmt.Sprintf("%s (build %s)", info.Label, info.ID)
}

// Check compares the in-memory client stamp with the marker in the exact bytes
// that will be uploaded or installed. ELF architecture validation is separate.
func Check(client string, artifact []byte) error {
	info, err := Parse(client)
	if err != nil {
		return fmt.Errorf("client is unversioned: %w", err)
	}
	matches := stampPattern.FindAll(artifact, 2)
	if len(matches) != 1 {
		return errors.New("executor has missing or ambiguous bundle identity; rebuild or install the whole matching bundle")
	}
	if !bytes.Equal(matches[0], []byte(client)) {
		other, _ := Parse(string(matches[0]))
		return fmt.Errorf("executor build %s does not match client build %s; rebuild or install the whole matching bundle", other.ID, info.ID)
	}
	return nil
}
