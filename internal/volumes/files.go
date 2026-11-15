package volumes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func ancestors(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrUnsafeStore
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return ErrUnsafeStore
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func private(info os.FileInfo, dir bool) bool {
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	return info.IsDir() == dir && (dir || info.Mode().IsRegular()) && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && (runtime.GOOS == "windows" || info.Mode().Perm() == mode) && owned(info)
}
func privateParent(dir string) bool {
	info, err := os.Lstat(filepath.Dir(dir))
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && owned(info) && (runtime.GOOS == "windows" || info.Mode().Perm()&0022 == 0)
}
func uniqueJSON(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 16 {
			return false
		}
		t, err := dec.Token()
		if err != nil {
			return false
		}
		d, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return false
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			for dec.More() {
				if !walk(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = dec.Token()
		return err == nil
	}
	if !walk(0) {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}
func replace(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".volume-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
