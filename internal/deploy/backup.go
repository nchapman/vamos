package deploy

import (
	"errors"
	"reflect"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/lifecycle"
)

var (
	errBackupPlanLegacy   = errors.New("backup plan requires a current identity-aware deployment; explicitly deploy with a current client before configuring older app identities")
	errBackupPlanAppState = errors.New("backup plan app state is unavailable, invalid, or belongs to another host; inspect app status and host storage")
	errBackupPlanRuntime  = errors.New("backup plan cannot verify stable effective runtimes; inspect app status, runtime ownership and host storage before applying")
)

func validateBackupPlanRequest(r Request) error {
	expected := Request{Version: 1, Action: "backup-plan", Host: r.Host, App: r.App, BackupPlan: r.BackupPlan}
	if r.BackupPlan == nil || !reflect.DeepEqual(r, expected) {
		return errors.New("backup plan requires an app-scoped plan-only request")
	}
	if err := lifecycle.ValidateConfig(r.Host); err != nil {
		return err
	}
	return backup.ValidatePlanRequest(r.App, *r.BackupPlan)
}

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
