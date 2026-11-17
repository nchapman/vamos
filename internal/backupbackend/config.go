// Package backupbackend provides pinned restic repository setup, not orchestration.
// Callers serialize host mutations and journal init intent. Cross-host init
// locking is not provided: create-new must use a fresh random operation prefix.
package backupbackend

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Version = "0.19.1"

type Config struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Region   string `json:"region"`
	Lookup   string `json:"lookup"`
}
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Password        string `json:"password"`
	CACertificate   string `json:"ca_certificate"`
}
type Identity struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

var errConfig = errors.New("invalid backup repository configuration")
var errCredentials = errors.New("invalid backup repository credentials")
var errIdentity = errors.New("invalid backup repository identity")
var errBackend = errors.New("backup repository operation failed")
var errTool = errors.New("backup repository tool verification failed")
var hostLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
var safePrefix = regexp.MustCompile(`^[A-Za-z0-9_/-]+$`)

func ValidateConfig(c Config) error {
	u, e := url.Parse(c.Endpoint)
	if e != nil || len(c.Endpoint) > 512 || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(c.Endpoint, "\\\r\n\t #") {
		return errConfig
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if !validEndpointAddr(ip) {
			return errConfig
		}
	} else {
		if len(host) > 253 || strings.Contains(host, ":") {
			return errConfig
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || !hostLabel.MatchString(label) {
				return errConfig
			}
		}
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return errConfig
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return errConfig
	}
	if len(c.Bucket) < 3 || len(c.Bucket) > 63 || !safeName.MatchString(c.Bucket) || net.ParseIP(c.Bucket) != nil {
		return errConfig
	}
	if len(c.Region) < 2 || len(c.Region) > 63 || !safeName.MatchString(c.Region) {
		return errConfig
	}
	if c.Lookup != "path" && c.Lookup != "dns" {
		return errConfig
	}
	if len(c.Prefix) == 0 || len(c.Prefix) > 512 || !safePrefix.MatchString(c.Prefix) {
		return errConfig
	}
	for _, p := range strings.Split(c.Prefix, "/") {
		if p == "" || p == "." || p == ".." || len(p) > 128 {
			return errConfig
		}
	}
	return nil
}

func validEndpointAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}
func ValidateCredentials(c Credentials) error {
	for i, v := range []string{c.AccessKeyID, c.SecretAccessKey, c.SessionToken} {
		if (i != 2 && v == "") || len(v) > 4096 {
			return errCredentials
		}
		for _, r := range v {
			if r < 33 || r > 126 {
				return errCredentials
			}
		}
	}
	// Existing restic repositories may use Unicode passphrases with spaces.
	// Passwords use a private file, never shell syntax or AWS environment values.
	if len(c.Password) == 0 || len(c.Password) > 4096 || !utf8.ValidString(c.Password) {
		return errCredentials
	}
	for _, r := range c.Password {
		if unicode.IsControl(r) {
			return errCredentials
		}
	}
	if len(c.CACertificate) > 16384 {
		return errCredentials
	}
	if c.CACertificate != "" {
		rest := strings.TrimSpace(c.CACertificate)
		if rest == "" {
			return errCredentials
		}
		for rest != "" {
			if !strings.HasPrefix(rest, "-----BEGIN CERTIFICATE-----") {
				return errCredentials
			}
			block, tail := pem.Decode([]byte(rest))
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return errCredentials
			}
			if _, e := x509.ParseCertificate(block.Bytes); e != nil {
				return errCredentials
			}
			rest = strings.TrimSpace(string(tail))
		}
	}
	return nil
}
func ValidateIdentity(i Identity) error {
	b, e := hex.DecodeString(i.ID)
	if e != nil || len(b) != 32 || i.Version != 2 {
		return errIdentity
	}
	return nil
}
