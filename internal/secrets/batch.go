package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// MaxBatchBytes bounds the worst-case escaped JSON private batch payload.
const MaxBatchBytes = MaxNames*MaxValueBytes*6 + 8192

// VersionFor derives a domain-separated immutable version for a batch member.
// Singleton operations use the operation identity directly instead.
func VersionFor(operation, name string) string {
	sum := sha256.Sum256([]byte("vamos/secrets/batch/v1\x00" + operation + "\x00" + name))
	return hex.EncodeToString(sum[:16])
}

func batchNames(values map[string][]byte) ([]string, error) {
	if len(values) < 1 || len(values) > MaxNames {
		return nil, invalid
	}
	names := make([]string, 0, len(values))
	for name, value := range values {
		if !ValidName(name) {
			return nil, invalid
		}
		if err := ValidateValue(value); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// EncodeBatch encodes validated secret values as a JSON object of strings.
func EncodeBatch(values map[string][]byte) ([]byte, error) {
	if _, err := batchNames(values); err != nil {
		return nil, err
	}
	strings := make(map[string]string, len(values))
	for name, value := range values {
		strings[name] = string(value)
	}
	data, err := json.Marshal(strings)
	if err != nil || len(data) > MaxBatchBytes {
		return nil, invalid
	}
	return data, nil
}

// DecodeBatch accepts only a bounded JSON object with exactly the requested names.
func DecodeBatch(data []byte, names []string) (map[string][]byte, error) {
	if len(data) > MaxBatchBytes || !utf8.Valid(data) || len(names) < 1 || len(names) > MaxNames {
		return nil, invalid
	}
	requested := make(map[string]bool, len(names))
	for _, name := range names {
		if !ValidName(name) || requested[name] {
			return nil, invalid
		}
		requested[name] = true
	}
	if !uniqueJSON(data) {
		return nil, invalid
	}
	var values map[string]*string
	if json.Unmarshal(data, &values) != nil || len(values) != len(names) {
		return nil, invalid
	}
	out := make(map[string][]byte, len(names))
	for name, value := range values {
		if !requested[name] || value == nil {
			return nil, invalid
		}
		v := []byte(*value)
		if err := ValidateValue(v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}
