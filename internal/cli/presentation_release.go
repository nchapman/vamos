package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/nchapman/vamos/internal/deploy"
)

func (p *presentation) releaseReport(result *deploy.RunResult, r appReport) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.section("Release command"))
	b.WriteString(p.field("Service", result.Service))
	b.WriteString(p.field("Run operation", result.Operation))
	b.WriteString(p.field("Phase", result.Phase))
	if result.ExitCode != nil {
		b.WriteString(p.field("Exit code", fmt.Sprint(*result.ExitCode)))
	}
	if result.Phase != "complete" || (result.ExitCode != nil && *result.ExitCode != 0) {
		b.WriteString(p.prose("Rollout is blocked. Resume observes this execution; it does not retry a started release command."))
	}
	b.WriteString(p.command(r.command("logs", "--run", result.Operation)))
	return b.String()
}

func printReleaseSummary(w io.Writer, result *deploy.RunResult) error {
	if result == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Release: operation=%q service=%q phase=%q\n", result.Operation, result.Service, result.Phase); err != nil {
		return err
	}
	if result.ExitCode != nil {
		_, err := fmt.Fprintf(w, "Release exit code: %d\n", *result.ExitCode)
		return err
	}
	return nil
}
