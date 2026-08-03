package domain

import (
	"strings"
	"time"
)

// Image is normalized image metadata.
//
// It is HarborMaster's own model, not the runtime's: nothing here mirrors a
// Docker SDK struct, so a different runtime adapter can populate the same
// fields without the API or the UI changing.
type Image struct {
	// ID is the content-addressable image ID, e.g. "sha256:abc...".
	ID string `json:"id"`
	// ShortID is ID truncated for display.
	ShortID string `json:"shortId"`
	// RepoTags are names in the local cache referencing this image. Empty for
	// an untagged ("dangling") image.
	RepoTags []string `json:"repoTags"`
	// RepoDigests are content-addressable manifest references. Usually present
	// only for images that were pulled from or pushed to a registry.
	RepoDigests []string `json:"repoDigests"`
	// CreatedAt is when the image was built. Zero when the runtime did not
	// report it.
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	Architecture string    `json:"architecture,omitempty"`
	OS           string    `json:"os,omitempty"`
	OSVersion    string    `json:"osVersion,omitempty"`
	Variant      string    `json:"variant,omitempty"`
	// Size is the total size in bytes of every layer.
	Size int64 `json:"size"`
	// Author and Comment are free-text provenance fields, often empty.
	Author  string `json:"author,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Labels declared by the image itself, distinct from container labels.
	Labels map[string]string `json:"labels,omitempty"`
}

// ImageRef is a parsed image reference as the operator wrote it.
//
// The original string is always preserved: parsing is best-effort, and an
// unparseable reference must still be displayable.
type ImageRef struct {
	// Raw is the reference exactly as the container declared it.
	Raw string `json:"raw"`
	// Repository is the name without tag or digest, e.g. "docker.io/library/nginx".
	Repository string `json:"repository,omitempty"`
	// Tag is the tag, when the reference carries one.
	Tag string `json:"tag,omitempty"`
	// Digest is the digest, when the reference is digest-pinned.
	Digest string `json:"digest,omitempty"`
}

// ParseImageRef splits a reference into repository, tag, and digest.
//
// It is deliberately lenient. A reference that cannot be split is returned
// with only Raw populated rather than producing an error: an odd reference is
// something to display, not something to fail an inventory refresh over.
func ParseImageRef(raw string) ImageRef {
	ref := ImageRef{Raw: raw}
	if raw == "" {
		return ref
	}

	remainder := raw

	// A digest, if present, is always the trailing "@sha256:..." component.
	if at := strings.LastIndex(remainder, "@"); at >= 0 {
		ref.Digest = remainder[at+1:]
		remainder = remainder[:at]
	}

	// A colon is only a tag separator when it comes after the final slash;
	// otherwise it is the port in a registry host such as "registry:5000/app".
	if colon := strings.LastIndex(remainder, ":"); colon >= 0 {
		if slash := strings.LastIndex(remainder, "/"); colon > slash {
			ref.Tag = remainder[colon+1:]
			remainder = remainder[:colon]
		}
	}

	ref.Repository = remainder

	// An untagged, undigested reference means "latest" by convention. Recording
	// it explicitly keeps the UI from showing a blank column.
	if ref.Tag == "" && ref.Digest == "" && ref.Repository != "" {
		ref.Tag = "latest"
	}
	return ref
}

// ShortenID truncates a content-addressable ID for display, preserving the
// algorithm prefix if one is present.
//
// Docker's own convention is 12 hex characters, which this matches.
func ShortenID(id string) string {
	const shortLen = 12

	trimmed := id
	if colon := strings.Index(trimmed, ":"); colon >= 0 {
		trimmed = trimmed[colon+1:]
	}
	if len(trimmed) > shortLen {
		return trimmed[:shortLen]
	}
	return trimmed
}
