package deploy

import (
	"strings"
	"unicode/utf8"

	"github.com/nchapman/vamos/internal/process"
)

// buildDiagnostic is only for build output, never runtime logs or inspect data.
// Exact app-local matching cannot protect arbitrary or encoded secret output.
func (m *Manager) buildDiagnostic(j *journal, out process.Result, cause error, limit int, requiredRefs ...bool) string {
	state, err := m.secretStore().Load(j.App, j.HostID)
	hasRefs := len(j.SecretRefs) > 0
	for _, service := range j.ServiceStates {
		if service != nil && len(service.SecretRefs) > 0 {
			hasRefs = true
		}
	}
	for _, release := range retainedReleases(j) {
		hasRefs = hasRefs || len(release.SecretRefs) > 0
	}
	for _, required := range requiredRefs {
		hasRefs = hasRefs || required
	}
	if err != nil || (state == nil && hasRefs) {
		return "build failed; diagnostic output withheld (secret store unavailable)"
	}
	clean := func(b []byte) string {
		if len(b) >= process.OutputLimit {
			if i := strings.IndexByte(string(b), '\n'); i >= 0 {
				b = b[i+1:]
			} else {
				b = nil
			}
		}
		return string(b)
	}
	text := clean(out.Stderr)
	if len(out.Stderr) == 0 {
		text = clean(out.Stdout)
	}
	if cause != nil {
		if text != "" {
			text = cause.Error() + "\n" + text
		} else {
			text = cause.Error()
		}
	}
	text = state.Redact(text)
	text = strings.ToValidUTF8(text, "?")
	const prefix = "build failed: "
	n := limit - len(prefix)
	if len(text) > n {
		text = text[len(text)-n:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	if text == "" {
		return "build failed; no retained diagnostic output"
	}
	return prefix + text
}
