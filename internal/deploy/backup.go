package deploy

import (
	"errors"
	"reflect"

	"github.com/nchapman/soar/internal/backup"
	"github.com/nchapman/soar/internal/lifecycle"
)

func validateBackupRequest(r Request) error {
	expected := Request{Version: 1, Action: "backup-repository", Host: r.Host, Backup: r.Backup}
	if r.Backup == nil || !reflect.DeepEqual(r, expected) {
		return errors.New("repository setup requires a host-only request")
	}
	if err := lifecycle.ValidateConfig(r.Host); err != nil {
		return err
	}
	return backup.ValidateRequest(*r.Backup)
}
