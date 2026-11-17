package deploy

import "slices"

// Bound response metadata independently of the number of expanded Docker
// bindings. Keep ranges compact and disclose omitted mapping entries explicitly.
const maxStatusPortMappings = 32

func recordedPublishedPorts(r *release) *PublishedPortStatus {
	if r == nil || r.Config == nil || len(r.Config.Ports) == 0 {
		return nil
	}
	mappings := slices.Clone(r.Config.Ports)
	slices.Sort(mappings)
	total := len(mappings)
	if len(mappings) > maxStatusPortMappings {
		mappings = mappings[:maxStatusPortMappings]
	}
	return &PublishedPortStatus{Mappings: mappings, Total: total}
}
