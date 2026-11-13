package cli

import (
	"fmt"
	"io"

	"github.com/nchapman/soar/internal/deploy"
)

func printDomainStatuses(w io.Writer, domains []deploy.DomainStatus) error {
	for _, d := range domains {
		if _, err := fmt.Fprintf(w, "Domain: https://%s route=%s DNS=%s certificate=%s HTTPS=%s\n", d.Hostname, d.Route, d.DNS, d.Certificate, d.HTTPS); err != nil {
			return err
		}
		note := ""
		switch {
		case d.Route == "pending":
			note = "Desired alias is not configured yet; DNS and certificate have not been checked."
		case d.Route == "unavailable":
			note = "Route is unavailable; DNS and certificate have not been checked."
		case d.DNS == "unknown":
			note = "Readiness has not been checked. Run status after the active operation completes."
		case d.DNS == "unverified":
			note = "DNS does not exclusively match public host interface addresses; NAT/CDN setups cannot be verified by this check."
		case d.DNS == "pending":
			note = "DNS resolution is pending."
		}
		if d.Certificate == "pending" {
			note += " A locally verified certificate is pending."
		}
		if d.Certificate == "ready" {
			note += " Certificate verified locally; this does not guarantee public reachability."
		}
		if note != "" {
			if _, err := fmt.Fprintln(w, note); err != nil {
				return err
			}
		}
	}
	return nil
}
