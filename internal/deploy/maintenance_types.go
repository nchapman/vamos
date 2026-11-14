package deploy

// Public previews leave headroom for response metadata within the 64 KiB transport.
// The journal retains its independent 2 MiB limit; new deletion authority lives
// in bounded immutable pages, not in the public arrays or the journal body.
const maxMaintenancePlanBytes = 32 << 10
const maxMaintenancePlanItems = 1024

// Plan is a bounded preview. --yes authorizes a freshly recomputed plan under
// the host lock, rather than binding an earlier preview to mutable resources.
type Plan struct {
	Action     string   `json:"action"`
	App        string   `json:"app"`
	Containers []string `json:"containers"`
	Images     []string `json:"images"`
	Archives   []string `json:"archives"`
	Network    string   `json:"network,omitempty"`
	Retained   []string `json:"retained"`
	// Totals is present only when detail arrays are incomplete. Counts are exact
	// for the captured plan, not estimates or counts of the displayed prefix.
	Totals            *PlanTotals `json:"totals,omitempty"`
	DetailsIncomplete bool        `json:"details_incomplete,omitempty"`
}

type PlanTotals struct {
	Containers int `json:"containers"`
	Images     int `json:"images"`
	Archives   int `json:"archives"`
	Retained   int `json:"retained"`
}

type plannedContainer struct {
	Name, ID, Image, Operation, Service, NetworkID string
	Replica                                        int `json:",omitempty"`
}
type plannedNetwork struct{ Name, ID string }
type plannedFile struct{ Name, SHA256 string }

// Maintenance preserves the deployment identity while recording a distinct,
// immutable destruction/cleanup plan. Replay may shrink work, never expand it.
type maintenance struct {
	Service            string `json:",omitempty"`
	Operation          string
	Action             string
	Plan               Plan
	Containers         []plannedContainer
	Images             []string
	Files              []plannedFile
	Network            *plannedNetwork
	OldCaddy, NewCaddy string
	Manifest           *maintenanceManifestRef `json:",omitempty"`
	ResourcesDone      bool                    `json:",omitempty"`
}
