package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/nchapman/vamos/internal/backup"
)

type backupKitFile struct {
	root *os.Root
	name string
	info os.FileInfo
}

func stageBackupKit(path string, kit backup.RecoveryKit) (*backupKitFile, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*backupKitFile, error) { root.Close(); return nil, e }
	dir, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	info, err := dir.Stat()
	dir.Close()
	if err != nil {
		return fail(err)
	}
	if runtime.GOOS != "windows" {
		value := reflect.Indirect(reflect.ValueOf(info.Sys())).FieldByName("Uid")
		if !value.IsValid() || value.Uint() != uint64(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
			return fail(errors.New("recovery parent must be caller-owned and not group/world writable"))
		}
	}
	name := filepath.Base(path)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(err)
	}
	err = json.NewEncoder(file).Encode(kit)
	if err == nil {
		err = file.Sync()
	}
	fi, statErr := file.Stat()
	closeErr := file.Close()
	if err == nil {
		err = statErr
	}
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncBackupDir(root)
	}
	if err != nil {
		return fail(err)
	}
	return &backupKitFile{root: root, name: name, info: fi}, nil
}
func syncBackupDir(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, e := root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (k *backupKitFile) update(kit backup.RecoveryKit) error {
	info, err := k.root.Lstat(k.name)
	if err != nil {
		return err
	}
	if !os.SameFile(k.info, info) || !info.Mode().IsRegular() {
		return errors.New("recovery file was replaced; original key retained")
	}
	suffix, err := backupRandom()
	if err != nil {
		return err
	}
	temp := k.name + "." + suffix
	f, err := k.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer k.root.Remove(temp)
	err = json.NewEncoder(f).Encode(kit)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	info, err = k.root.Lstat(k.name)
	if err != nil {
		return err
	}
	if !os.SameFile(k.info, info) {
		return errors.New("recovery file was replaced")
	}
	if err = k.root.Rename(temp, k.name); err != nil {
		return err
	}
	return syncBackupDir(k.root)
}
