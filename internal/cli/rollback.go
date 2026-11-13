package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/nchapman/soar/internal/deploy"
)

func printRollbackPreview(w io.Writer, p *deploy.RollbackPreview) error {
	if _, err := fmt.Fprintf(w, "Rollback target operation: %s\n", p.TargetOperation); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Rollback restores code/configuration only, not data or schemas. Confirm the old code is compatible with current data."); err != nil {
		return err
	}
	for _, s := range p.Services {
		if s.ExcludedReason != "" {
			if _, err := fmt.Fprintf(w, "Service %q excluded: %s\n", s.Name, s.ExcludedReason); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "Service %q: %s -> %s (downtime=%t)\n", s.Name, s.CurrentImage, s.TargetImage, s.Downtime); err != nil {
			return err
		}
		names := make([]string, 0, len(s.SecretVersions))
		for name := range s.SecretVersions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, err := fmt.Fprintf(w, "  Captured secret %q: version %s\n", name, s.SecretVersions[name]); err != nil {
				return err
			}
		}
		if len(names) > 0 {
			if _, err := fmt.Fprintln(w, "Warning: captured credentials may have been revoked; rollback does not restore their validity."); err != nil {
				return err
			}
		}
	}
	if p.NeedsConfirmation {
		_, err := fmt.Fprintln(w, "Preview only: rerun rollback with --yes to apply; use --allow-downtime to authorize stop/start where required.")
		return err
	}
	return nil
}
