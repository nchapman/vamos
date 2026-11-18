package bridgeacl

import (
	"bytes"
	"encoding/base64"
	"errors"
	"regexp"

	"golang.org/x/crypto/ssh"
)

var publicLine = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) ([A-Za-z0-9+/=]+)(?: [ -~]{0,511})?$`)
var errInvalidKey = errors.New("invalid SSH public key")

func fingerprint(line string) (string, error) {
	if len(line) > 4096 {
		return "", errInvalidKey
	}
	m := publicLine.FindStringSubmatch(line)
	if m == nil {
		return "", errInvalidKey
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(m[2])
	if err != nil {
		return "", errInvalidKey
	}
	k, err := ssh.ParsePublicKey(blob)
	if err != nil || k.Type() != m[1] || !bytes.Equal(blob, k.Marshal()) {
		return "", errInvalidKey
	}
	return ssh.FingerprintSHA256(k), nil
}
