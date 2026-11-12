// Package appstate defines the small completed-destruction receipt shared by
// app deployment and host teardown without coupling their state machines.
package appstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

// Tombstone is the entire state.json after a completed app destruction.
// It contains no releases, source archives, configuration, or pending intent.
type Tombstone struct {
	Version   int    `json:"version"`
	App       string `json:"app"`
	Operation string `json:"operation"`
	Phase     string `json:"phase"`
	HostID    string `json:"HostID"`
}

var appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var identity = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ValidName(app string) bool { return len(app) <= 63 && appName.MatchString(app) }

func (t Tombstone) Validate(app, hostID string) error {
	if t.Version != 1 || t.Phase != "destroyed" || t.App != app || !ValidName(app) || !identity.MatchString(t.Operation) || !identity.MatchString(t.HostID) || t.HostID != hostID {
		return errors.New("invalid completed app destruction receipt")
	}
	return nil
}

func Decode(r io.Reader, app, hostID string) (Tombstone, error) {
	var t Tombstone
	b, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil {
		return t, err
	}
	if len(b) > 4096 {
		return t, errors.New("app destruction receipt exceeds 4 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&t); err != nil {
		return t, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return t, errors.New("trailing app destruction receipt data")
	}
	return t, t.Validate(app, hostID)
}
