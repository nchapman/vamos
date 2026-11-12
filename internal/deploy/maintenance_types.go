package deploy

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
}

type plannedContainer struct{ Name, ID, Image, Operation, NetworkID string }
type plannedNetwork struct{ Name, ID string }
type plannedFile struct{ Name, SHA256 string }

// Maintenance preserves the deployment identity while recording a distinct,
// immutable destruction/cleanup plan. Replay may shrink work, never expand it.
type maintenance struct {
	Operation          string
	Action             string
	Plan               Plan
	Containers         []plannedContainer
	Images             []string
	Files              []plannedFile
	Network            *plannedNetwork
	OldCaddy, NewCaddy string
}
