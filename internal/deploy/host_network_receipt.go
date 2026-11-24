package deploy

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A build IID alone cannot distinguish two services sharing a build. Bind the
// captured network intent to one canonical runtime name in the agent's private
// app directory. Use the existing IID inventory, retention and destroy barrier.
func (m *Manager) hostReceiptPath(j *journal, service, name, op string) string {
	key := sha256.Sum256([]byte(strings.Join([]string{"vamos:network-receipt:v1", j.HostID, j.App, service, name, "host", op}, "\x00")))
	return filepath.Join(m.dir(j.App), fmt.Sprintf("%s.%x.image.iid", op, key[:16]))
}

func readHostReceipt(path string) (string, error) {
	if err := inventoryPath(path, false, true); err != nil {
		return "", err
	}
	b, _, err := resourceRead(path, 128)
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(string(b), "\n")
	if !imageID.MatchString(id) {
		return "", errors.New("invalid host network receipt")
	}
	return id, nil
}

// Caller holds the deployment lock and has validated captured intent. Never
// replace an existing receipt, even if the preceding Docker create failed.
func (m *Manager) recordHostReceipt(j *journal, r runtimeRef) error {
	path := m.hostReceiptPath(j, r.Service, r.Container, r.Operation)
	id, err := readHostReceipt(path)
	if errors.Is(err, os.ErrNotExist) {
		return atomic(path, []byte(r.Image+"\n"), 0600)
	}
	if err != nil {
		return err
	}
	if id != r.Image {
		return errors.New("changed host network receipt")
	}
	return nil
}
