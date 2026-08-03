package domain

import "time"

// Host is a container runtime endpoint HarborMaster inventories.
//
// Multi-host management is not implemented; exactly one host row exists, with
// ID LocalHostID. The type exists so that the repositories, the foreign keys,
// and the API are already keyed by host, and adding a second host later is a
// data change rather than a schema migration.
type Host struct {
	ID string `json:"id"`
	// Name is an operator-facing label.
	Name string `json:"name"`
	// Runtime identifies the adapter that produced this inventory, e.g. "docker".
	Runtime string `json:"runtime"`
	// APIVersion is the runtime's reported API version, when known.
	APIVersion string `json:"apiVersion,omitempty"`
	// OSType is the runtime's reported operating system, when known.
	OSType    string    `json:"osType,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LocalHostID identifies the single local runtime endpoint.
const LocalHostID = "local"

// RuntimeDocker is the only runtime implemented.
const RuntimeDocker = "docker"
