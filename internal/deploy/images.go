package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/distribution/reference"
	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/config"
)

// imageKey separates image sources from build specifications and coalesces
// familiar Docker names with their fully qualified equivalents.
func imageKey(cfg config.Service) string {
	if cfg.Build != nil {
		return buildcontext.BuildKey(*cfg.Build)
	}
	source := cfg.Image
	if ref, err := reference.ParseNormalizedNamed(source); err == nil {
		source = reference.TagNameOnly(ref).String()
	}
	sum := sha256.Sum256([]byte("soar:image-source\x00" + source))
	return hex.EncodeToString(sum[:16])
}

func validImageReference(ref string) error {
	if len(ref) == 0 || len(ref) > 1024 || strings.HasPrefix(ref, "-") || strings.Contains(ref, "://") || strings.ContainsFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("invalid image reference")
	}
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return errors.New("invalid image reference")
	}
	return nil
}

func imageVolumesAllowed(im *inspection, cfg *config.Service, allowNamed bool) error {
	if im == nil {
		return errors.New("missing image inspection")
	}
	if len(im.Config.Volumes) == 0 {
		return nil
	}
	if !allowNamed || cfg == nil || cfg.Volume == nil || len(im.Config.Volumes) != 1 {
		return errors.New("image declares anonymous volumes")
	}
	for mount := range im.Config.Volumes {
		if mount != cfg.Volume.Mount || !path.IsAbs(mount) || path.Clean(mount) != mount || mount == "/" || strings.ContainsFunc(mount, unicode.IsControl) {
			return errors.New("image volume does not match named mount")
		}
	}
	return nil
}

// resolvePrebuilt records the immutable identity before exposing it to journal
// callers. Once recorded, a retry must never consult or refresh the source tag.
func (m *Manager) resolvePrebuilt(ctx context.Context, j *journal, cfg config.Service) (string, error) {
	if j == nil || !operation.MatchString(j.Operation) || !label.MatchString(j.App) || len(j.App) > 63 || cfg.Build != nil {
		return "", errors.New("invalid image resolution configuration")
	}
	if err := validImageReference(cfg.Image); err != nil {
		return "", err
	}
	p := filepath.Join(m.dir(j.App), j.Operation+"."+imageKey(cfg)+".image.iid")
	if safe(p) != nil {
		return "", errors.New("unsafe image identity file")
	}
	id := ""
	st, err := os.Lstat(p)
	if err == nil {
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 128 {
			return "", errors.New("unsafe image identity file")
		}
		f, err := os.Open(p)
		if err != nil {
			return "", errors.New("read image identity failed")
		}
		b, err := io.ReadAll(io.LimitReader(f, 129))
		f.Close()
		if err != nil || len(b) > 128 {
			return "", errors.New("read image identity failed")
		}
		id = strings.TrimSpace(string(b))
		if !imageID.MatchString(id) {
			return "", errors.New("invalid captured image identity")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("read image identity failed")
	} else {
		if _, err := m.Run(ctx, "docker", []string{"pull", "--quiet", cfg.Image}, nil); err != nil {
			return "", errors.New("image pull failed")
		}
		im, err := m.inspect(ctx, "image", cfg.Image)
		if err != nil || !imageID.MatchString(im.ID) {
			return "", errors.New("resolve image identity failed")
		}
		id = im.ID
		// Persist even before admission checks: a failed check cannot authorize a
		// subsequent retry to replace this identity with a newer mutable tag.
		if atomic(p, []byte(id+"\n"), 0600) != nil {
			return "", errors.New("persist image identity failed")
		}
	}
	im, err := m.inspect(ctx, "image", id)
	if err != nil {
		return "", errors.New("inspect captured image failed")
	}
	if im.ID != id {
		return "", errors.New("captured image identity mismatch")
	}
	if err := imageVolumesAllowed(im, &cfg, true); err != nil {
		return "", err
	}
	return id, nil
}
