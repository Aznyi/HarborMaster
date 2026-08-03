package domain

import "time"

// Network is a normalized container network.
type Network struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Driver string `json:"driver,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// Internal networks have no external connectivity.
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	IPv6       bool              `json:"ipv6"`
	CreatedAt  time.Time         `json:"createdAt,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// Subnets are the IPAM-configured CIDR ranges.
	Subnets []string `json:"subnets,omitempty"`
}

// Volume is a normalized storage volume.
//
// HarborMaster records volume metadata only. It never reads volume contents:
// a volume can hold anything, including credentials, and inventorying it is
// not a reason to open it.
type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"`
	CreatedAt  time.Time         `json:"createdAt,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// Options are driver-specific creation options.
	Options map[string]string `json:"options,omitempty"`
}
