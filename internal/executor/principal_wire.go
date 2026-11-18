package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// The privileged principal wire format has no nullable values. Unlike the
// default struct decoder it also rejects duplicate and case-aliased fields.
func decodePrincipalJSON(data []byte, dst any) error {
	bad := errors.New("invalid principal protocol document")
	if !utf8.Valid(data) {
		return bad
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 4 {
			return bad
		}
		tok, err := dec.Token()
		if err != nil || tok == nil {
			return bad
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				s, ok := key.(string)
				if err != nil || !ok || strings.ToLower(s) != s || seen[s] {
					return bad
				}
				seen[s] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func DecodePrincipalResponse(data []byte) (PrincipalResponse, error) {
	var reply PrincipalResponse
	if len(data) > MaxPrincipalOutput {
		return reply, errors.New("principal response exceeds limit")
	}
	err := decodePrincipalJSON(data, &reply)
	return reply, err
}
