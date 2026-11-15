package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/secrets"
)

func (m *Manager) readRunResponse(ctx context.Context, r Request, j *journal, state *secrets.State, active bool) (Response, error) {
	resp := Response{Version: 1}
	result, err := m.runStatus(ctx, j, r.RunOperation)
	if err != nil {
		return resp, err
	}
	result.Output = ""
	s := m.multiStatus(ctx, j, state, active, "")
	s.Run = result
	resp.Status = &s
	if r.Action == "logs" {
		resp.LogService, resp.LogContainer = result.Service, result.Container
		resp.Logs, err = m.runLogs(ctx, j, r.RunOperation)
	}
	return resp, err
}

// Status discovers the latest submitted command even if its submission response
// was lost. Output belongs to explicit logs/run responses, not metadata status.
func (m *Manager) enrichRunStatus(ctx context.Context, j *journal, status *Status) {
	paths, err := filepath.Glob(filepath.Join(m.dir(j.App), "*.run.json"))
	if err != nil || len(paths) > 256 {
		status.LastError = diagnostic(status.LastError + "; run history unavailable")
		return
	}
	var latest string
	var stamp time.Time
	for _, p := range paths {
		info, e := os.Lstat(p)
		if e != nil {
			status.LastError = diagnostic(status.LastError + "; run history unavailable")
			return
		}
		if latest == "" || info.ModTime().After(stamp) {
			latest, stamp = p, info.ModTime()
		}
	}
	if latest == "" {
		return
	}
	result, err := m.runStatus(ctx, j, strings.TrimSuffix(filepath.Base(latest), ".run.json"))
	if err != nil {
		status.LastError = diagnostic(fmt.Sprint(status.LastError, "; run metadata unavailable"))
		return
	}
	result.Output = ""
	status.Run = result
}
