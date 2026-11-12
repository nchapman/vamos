// Package host owns the read-only host probe and deployment-host policy.
package host

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed inspect.sh
var collector string

// Collector is fixed code, identical for bootstrap SSH and an installed executor.
func Collector() string { return collector }

// Collect is the external execution boundary. It must enforce context cancellation
// and output limits and must not return successful output after a failed process.
type Collect func(context.Context) ([]byte, error)

type Report struct {
	Kernel       string   `json:"kernel"`
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	Version      string   `json:"os_version"`
	Systemd      bool     `json:"systemd"`
	UID          uint32   `json:"uid"`
	Root         bool     `json:"root"`
	Supported    bool     `json:"supported"`
	Problems     []string `json:"problems"`
}

func Inspect(ctx context.Context, collect Collect) (*Report, error) {
	output, err := collect(ctx)
	if err != nil {
		return nil, err
	}
	return Parse(output)
}

const maxOSRelease = 16384

var (
	scalarPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	idPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	versionPattern = regexp.MustCompile(`^([0-9]{2})\.([0-9]{2})$`)
	uidPattern     = regexp.MustCompile(`^[0-9]+$`)
)

// Parse rejects contaminated/truncated probe output rather than looking for a
// recognizable fragment within login banners or arbitrary remote output.
func Parse(output []byte) (*Report, error) {
	fields := strings.SplitN(string(output), "\n", 8)
	if len(fields) != 8 || fields[0] != "soar-inspect-v1" {
		return nil, errors.New("invalid host probe response (expected soar-inspect-v1 framing; check shell startup output)")
	}
	for _, field := range fields[1:4] {
		if !scalarPattern.MatchString(field) {
			return nil, errors.New("invalid scalar in host probe response")
		}
	}
	if !scalarPattern.MatchString(fields[4]) || (fields[5] != "yes" && fields[5] != "no") {
		return nil, errors.New("invalid systemd evidence in host probe response")
	}
	uid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil || !uidPattern.MatchString(fields[3]) {
		return nil, errors.New("invalid UID in host probe response")
	}
	if len(fields[7]) > maxOSRelease {
		return nil, errors.New("os-release exceeds the 16 KiB inspection limit")
	}
	if fields[6] != "present" && fields[6] != "missing" {
		return nil, errors.New("invalid os-release status in host probe response")
	}
	if fields[6] == "missing" && fields[7] != "" {
		return nil, errors.New("unexpected data after missing os-release status")
	}

	r := &Report{
		Kernel: fields[1], Architecture: fields[2],
		UID: uint32(uid), Root: uid == 0,
		Systemd:  fields[4] == "systemd" && fields[5] == "yes",
		Problems: []string{},
	}
	if r.Kernel != "Linux" {
		r.Problems = append(r.Problems, "deployment hosts must run Linux (Ubuntu 24.04+)")
	}
	switch r.Architecture {
	case "x86_64":
		r.Architecture = "amd64"
	case "aarch64", "arm64":
		r.Architecture = "arm64"
	default:
		r.Problems = append(r.Problems, "host architecture must be amd64 or arm64")
	}
	if fields[6] == "missing" {
		r.Problems = append(r.Problems, "/etc/os-release is missing or unreadable; Ubuntu 24.04+ could not be verified")
	} else {
		r.OS, r.Version, err = parseOSRelease(fields[7])
		if err != nil {
			r.Problems = append(r.Problems, "invalid /etc/os-release: "+err.Error())
		} else {
			if r.OS != "ubuntu" {
				r.Problems = append(r.Problems, "deployment hosts must have ID=ubuntu; other distributions and derivatives are unsupported")
			}
			if !supportedVersion(r.Version) {
				r.Problems = append(r.Problems, "Ubuntu VERSION_ID must be a recognized year.month release of 24.04 or newer")
			}
		}
	}
	if !r.Systemd {
		r.Problems = append(r.Problems, "systemd must be PID 1, with /run/systemd/system and systemctl available")
	}
	r.Supported = len(r.Problems) == 0
	return r, nil
}

// Only the two identity fields affect policy. Treat their values as data, never
// shell expressions; unrelated os-release fields need not be interpreted.
func parseOSRelease(data string) (string, string, error) {
	values := make(map[string]string)
	if strings.ContainsRune(data, '\x00') {
		return "", "", errors.New("NUL is not allowed")
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", errors.New("expected assignment lines")
		}
		if key != "ID" && key != "VERSION_ID" {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", fmt.Errorf("duplicate %s", key)
		}
		value := raw
		if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
			value = raw[1 : len(raw)-1]
		}
		if (key == "ID" && !idPattern.MatchString(value)) || (key == "VERSION_ID" && !versionPattern.MatchString(value)) {
			return "", "", fmt.Errorf("unrecognized %s value", key)
		}
		values[key] = value
	}
	if values["ID"] == "" || values["VERSION_ID"] == "" {
		return "", "", errors.New("ID and VERSION_ID are required")
	}
	return values["ID"], values["VERSION_ID"], nil
}

func supportedVersion(version string) bool {
	parts := versionPattern.FindStringSubmatch(version)
	if parts == nil {
		return false
	}
	year, _ := strconv.Atoi(parts[1])
	month, _ := strconv.Atoi(parts[2])
	return month >= 1 && month <= 12 && (year > 24 || (year == 24 && month >= 4))
}
