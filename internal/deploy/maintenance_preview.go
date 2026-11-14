package deploy

import (
	"encoding/json"
	"errors"
)

// Bound the whole response, not just the preview: status can contain substantial
// per-service, volume and secret metadata alongside an otherwise valid plan.
func boundMaintenanceResponse(resp Response, err error) (Response, error) {
	b, e := json.Marshal(resp)
	if e == nil && len(b)+1 <= 64<<10 {
		return resp, err
	}
	err = errors.Join(err, errors.New("maintenance response exceeds 64 KiB; inspect scoped status"))
	var status *Status
	if resp.Status != nil {
		status = &Status{App: resp.Status.App, Operation: resp.Status.Operation, Phase: resp.Status.Phase}
	}
	return Response{Version: 1, Status: status, Error: diagnostic(err.Error()), ErrorCode: ErrorCode(err)}, err
}

// Presentation limits never authorize or limit deletion. Private records remain
// complete; the public arrays are deterministic prefixes with exact counts.
func boundMaintenancePreview(p *Plan) error {
	if p.DetailsIncomplete || p.Totals != nil {
		return errors.New("preview already bounded")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	total := len(p.Containers) + len(p.Images) + len(p.Archives) + len(p.Retained)
	if len(b) <= maxMaintenancePlanBytes && total <= maxMaintenancePlanItems {
		return nil
	}
	p.Totals = &PlanTotals{len(p.Containers), len(p.Images), len(p.Archives), len(p.Retained)}
	p.DetailsIncomplete = true
	for _, list := range []*[]string{&p.Containers, &p.Images, &p.Archives, &p.Retained} {
		if len(*list) > 64 {
			*list = (*list)[:64]
		}
	}
	for {
		b, err = json.Marshal(p)
		if err != nil {
			return err
		}
		if len(b) <= maxMaintenancePlanBytes {
			return nil
		}
		// Also handles an individually oversized explanation without emitting an
		// invalid transport payload. Category order is part of the stable preview.
		removed := false
		for _, list := range []*[]string{&p.Retained, &p.Archives, &p.Images, &p.Containers} {
			if len(*list) > 0 {
				*list = (*list)[:len(*list)-1]
				removed = true
				break
			}
		}
		if !removed {
			return errors.New("maintenance metadata exceeds plan limit")
		}
	}
}

func maintenancePreviewMatches(w *maintenance) bool {
	p := w.Plan
	if p.DetailsIncomplete {
		t := p.Totals
		if t == nil || t.Containers != len(w.Containers) || t.Images != len(w.Images) || t.Archives != len(w.Files) || t.Retained < len(p.Retained) {
			return false
		}
		if len(p.Containers) > t.Containers || len(p.Images) > t.Images || len(p.Archives) > t.Archives {
			return false
		}
		if len(p.Containers) == t.Containers && len(p.Images) == t.Images && len(p.Archives) == t.Archives && len(p.Retained) == t.Retained {
			return false
		}
	} else if p.Totals != nil || len(p.Containers) != len(w.Containers) || len(p.Images) != len(w.Images) || len(p.Archives) != len(w.Files) {
		return false
	}
	for i, n := range p.Containers {
		if n != w.Containers[i].Name {
			return false
		}
	}
	for i, n := range p.Images {
		if n != w.Images[i] {
			return false
		}
	}
	for i, n := range p.Archives {
		if n != w.Files[i].Name {
			return false
		}
	}
	return true
}
