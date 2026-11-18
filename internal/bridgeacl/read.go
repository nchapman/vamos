// Package bridgeacl reads and updates the installed SSH bridge ACL.
package bridgeacl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nchapman/vamos/internal/agentproto"
)

const maxFile = 1 << 20

// ReadRegularFile reads an owned file under an owned, non-writable directory.
// Ownership is checked against the effective UID (root for the installed agent).
func ReadRegularFile(path string, maxBytes int64, label string) ([]byte, error) {
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || !owned(parent) {
		return nil, fmt.Errorf("unsafe parent for %s", label)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || !owned(before) || before.Size() > maxBytes {
		return nil, fmt.Errorf("unsafe or oversized %s", label)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || !owned(after) {
		return nil, fmt.Errorf("%s changed", label)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("oversized %s", label)
	}
	return data, err
}

func ReadPrincipals(path string) (map[string]agentproto.Principal, error) {
	data, err := ReadRegularFile(path, maxFile, "principals file")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var principals []agentproto.Principal
	if err := dec.Decode(&principals); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("principals file must contain exactly one JSON document")
	}
	result := make(map[string]agentproto.Principal, len(principals))
	for _, p := range principals {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, ok := result[p.ID]; ok {
			return nil, errors.New("duplicate principal ID")
		}
		result[p.ID] = p
	}
	return result, nil
}

var forced = regexp.MustCompile(`^restrict,command="(?:/usr/local/lib/vamos/vamos-agent|/usr/local/lib/vamos/executor) internal-agent bridge --principal ([A-Za-z0-9][A-Za-z0-9._:@/-]{0,127})" (.+)$`)

type keyLine struct {
	id, fp string
	index  int
}

func readKeys(path string, ps map[string]agentproto.Principal) ([]string, []keyLine, error) {
	data, err := ReadRegularFile(path, maxFile, "authorized_keys")
	if err != nil {
		return nil, nil, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	var keys []keyLine
	for i, raw := range lines {
		line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := forced.FindStringSubmatch(line)
		if match == nil {
			return nil, nil, fmt.Errorf("authorized key line %d is not a restricted Vamos bridge key", i+1)
		}
		if _, ok := ps[match[1]]; !ok {
			return nil, nil, fmt.Errorf("authorized key line %d names unknown principal %q", i+1, match[1])
		}
		fp, err := fingerprint(match[2])
		if err != nil {
			return nil, nil, fmt.Errorf("authorized key line %d: %w", i+1, err)
		}
		keys = append(keys, keyLine{match[1], fp, i})
	}
	return lines, keys, nil
}

func ValidateAuthorizedKeys(path string, ps map[string]agentproto.Principal) error {
	_, _, err := readKeys(path, ps)
	return err
}
