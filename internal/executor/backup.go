package executor

import (
	"errors"
	"io"
	"os"

	"github.com/nchapman/vamos/internal/backup"
)

func readRepositoryFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > backup.MaxPrivateBytes {
		return nil, errors.New("invalid private repository file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private repository file unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private repository file changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, backup.MaxPrivateBytes+1))
	if err != nil {
		return nil, errors.New("private repository file unavailable")
	}
	if _, err = backup.DecodeCredentials(data); err != nil {
		return nil, err
	}
	return data, nil
}
