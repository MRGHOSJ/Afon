package probe

import "time"

// SchemaVersion identifies the profile shape; bump when sections change.
const SchemaVersion = "afon.dev/probe/v1"

// ClusterProfile is a pure fact sheet about a cluster. It must never contain
// judgments or findings those live in a separate checks layer that consumes
// saved profiles.
type ClusterProfile struct {
	SchemaVersion string    `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	// DurationMs is the wall time of the whole Probe() call. Probes run in
	// parallel, so it tracks the slowest section plus client startup.
	DurationMs int64                    `json:"durationMs,omitempty"`
	Identity   Identity                 `json:"identity"`
	Version    VersionInfo              `json:"version"`
	Discovery  DiscoveryInfo            `json:"discovery"`
	Health     HealthInfo               `json:"health"`
	Nodes      NodesSection             `json:"nodes"`
	Access     AccessInfo               `json:"access"`
	Namespace  NamespaceInfo            `json:"namespace"`
	Networking NetworkingInfo           `json:"networking"`
	Storage    StorageInfo              `json:"storage"`
	Sections   map[string]SectionStatus `json:"sections"`
	Errors     []ProbeError             `json:"errors,omitempty"`
}
