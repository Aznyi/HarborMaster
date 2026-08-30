package domain

import (
	"errors"
	"net"
	"strings"
)

// Image reference normalization, and the registry vocabulary.
//
// # Why normalization is a security control here
//
// This is the only place a registry HOST is ever produced. Phase 6 makes
// HarborMaster's first outbound network requests, and the single most important
// property of that feature is that it can only ever talk to a host derived from
// an image reference that a container in the inventory actually declares.
//
// Nothing downstream accepts a host, a URL, or a scheme from a caller. The API
// has no parameter that reaches a network request; the refresh endpoint
// schedules work over the existing inventory and takes no target. So the
// question "where can HarborMaster be made to connect?" has exactly one answer,
// and it is answered in this file.
//
// NormalizeImageRef is deliberately STRICT where ParseImageRef is lenient.
// ParseImageRef exists to display an odd reference; this exists to decide
// whether a reference may be turned into an HTTPS request, and it refuses
// anything it cannot vouch for.

// ErrUnsupportedReference reports a reference that cannot safely become a
// registry request.
//
// Not a parse failure: the reference may be perfectly valid and simply name
// something this phase will not contact, such as a local registry. The caller
// records the image as unsupported rather than failed, because there is nothing
// to retry.
var ErrUnsupportedReference = errors.New("image reference is not supported for registry lookup")

// RegistryKind identifies the provider that serves a registry host.
//
// A closed vocabulary, and deliberately small. The distribution API is the same
// everywhere; what differs is host defaulting, repository shape, and how an
// anonymous pull token is obtained. See internal/registry.
type RegistryKind string

// Registry kinds.
const (
	// RegistryDockerHub is Docker Hub, which needs the most special handling:
	// the familiar name has no host at all, official images live under an
	// implicit "library" namespace, and the API host differs from the name.
	RegistryDockerHub RegistryKind = "dockerhub"
	// RegistryGHCR is the GitHub Container Registry.
	RegistryGHCR RegistryKind = "ghcr"
	// RegistryOCI is any other registry speaking the OCI distribution API.
	RegistryOCI RegistryKind = "oci"
	// RegistryUnknown is used for a reference that names no contactable
	// registry.
	RegistryUnknown RegistryKind = "unknown"
)

// RegistryKinds lists every kind.
var RegistryKinds = []RegistryKind{
	RegistryDockerHub, RegistryGHCR, RegistryOCI, RegistryUnknown,
}

// ValidRegistryKind reports whether name is a known kind.
func ValidRegistryKind(name string) bool {
	for _, kind := range RegistryKinds {
		if string(kind) == name {
			return true
		}
	}
	return false
}

// Well-known hosts.
const (
	// DockerHubFamiliarHost is the host that appears in a canonical reference.
	DockerHubFamiliarHost = "docker.io"
	// DockerHubAPIHost is where the distribution API actually lives, which is
	// not the same name.
	DockerHubAPIHost = "registry-1.docker.io"
	// DockerHubLegacyHost is the pre-2015 spelling, still seen in the wild.
	DockerHubLegacyHost = "index.docker.io"
	// DockerHubLibraryNamespace is the implicit namespace of official images.
	DockerHubLibraryNamespace = "library"
	// GHCRHost is the GitHub Container Registry.
	GHCRHost = "ghcr.io"
)

// Reference bounds.
//
// A reference comes from a container's configuration, which HarborMaster does
// not control, so its parts are bounded before any of them becomes part of a
// URL. The limits are far above any real reference.
const (
	MaxReferenceBytes  = 512
	MaxRepositoryBytes = 255
	MaxTagBytes        = 128
	MaxDigestBytes     = 256
)

// NormalizedRef is a reference resolved to its canonical, contactable form.
//
// Every field is derived, never supplied. In particular APIHost is the ONLY
// value that becomes a network destination, and it is either a well-known
// constant or the host component of the reference itself after validation.
type NormalizedRef struct {
	// Raw is the reference exactly as the container declared it.
	Raw string `json:"raw"`
	// Canonical is the fully qualified form, e.g.
	// "docker.io/library/nginx:1.25". Stable, so it is safe as a cache key.
	Canonical string `json:"canonical"`
	// Familiar is the short form an operator recognises, e.g. "nginx:1.25".
	Familiar string `json:"familiar"`

	Kind RegistryKind `json:"registryKind"`
	// Host is the registry as it appears in the canonical reference.
	Host string `json:"registry"`
	// APIHost is where the distribution API is served. Differs from Host only
	// for Docker Hub.
	APIHost string `json:"apiHost"`

	// Namespace is the owner segment ("library", an organisation, a user), and
	// Name the final path segment. Path is the full repository path used in the
	// API, which is Namespace/Name for a two-part repository and may be deeper.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Path      string `json:"repository"`

	Tag string `json:"tag,omitempty"`
	// Digest is set when the reference is digest-pinned.
	Digest string `json:"digest,omitempty"`
}

// Pinned reports whether the reference names an immutable digest.
//
// A digest-pinned reference cannot move, so the only meaningful question for it
// is whether a newer TAG exists -- never whether "the tag" changed.
func (r NormalizedRef) Pinned() bool { return r.Digest != "" }

// NormalizeImageRef resolves a raw reference to its canonical, contactable
// form.
//
// Returns ErrUnsupportedReference for anything this phase will not contact. The
// refusals are the security-relevant part, so they are listed explicitly:
//
//   - An empty or over-long reference.
//   - A host that is an IP LITERAL. A registry addressed by IP cannot be
//     distinguished from an SSRF target, and no public registry needs one.
//   - localhost, or any name without a dot, used as a host. Both are how a
//     reference reaches something inside the deployment's own network.
//   - A host carrying a PORT. A non-default port is a strong signal of a local
//     or internal registry, which this phase does not support.
//   - A host carrying userinfo, a path, or anything else that is not purely a
//     hostname. This is what stops "evil.example.com#@registry.example.com"
//     shapes from surviving into a URL.
//
// A refused reference is not an error the caller should retry; it is a
// statement that the image is out of scope.
func NormalizeImageRef(raw string) (NormalizedRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > MaxReferenceBytes {
		return NormalizedRef{}, ErrUnsupportedReference
	}

	parsed := ParseImageRef(trimmed)
	if parsed.Repository == "" {
		return NormalizedRef{}, ErrUnsupportedReference
	}
	if len(parsed.Repository) > MaxRepositoryBytes ||
		len(parsed.Tag) > MaxTagBytes || len(parsed.Digest) > MaxDigestBytes {
		return NormalizedRef{}, ErrUnsupportedReference
	}
	if parsed.Digest != "" && !validDigest(parsed.Digest) {
		return NormalizedRef{}, ErrUnsupportedReference
	}
	if parsed.Tag != "" && !validTag(parsed.Tag) {
		return NormalizedRef{}, ErrUnsupportedReference
	}

	host, remainder := splitRegistryHost(parsed.Repository)

	ref := NormalizedRef{
		Raw:    trimmed,
		Tag:    parsed.Tag,
		Digest: parsed.Digest,
	}

	switch host {
	case "", DockerHubFamiliarHost, DockerHubLegacyHost:
		ref.Kind = RegistryDockerHub
		ref.Host = DockerHubFamiliarHost
		ref.APIHost = DockerHubAPIHost
		// An official image has no namespace in its familiar form. Docker's own
		// rule: a single-segment path means the implicit library namespace.
		if !strings.Contains(remainder, "/") {
			remainder = DockerHubLibraryNamespace + "/" + remainder
		}
	case GHCRHost:
		ref.Kind = RegistryGHCR
		ref.Host = GHCRHost
		ref.APIHost = GHCRHost
	default:
		if !contactableHost(host) {
			return NormalizedRef{}, ErrUnsupportedReference
		}
		ref.Kind = RegistryOCI
		ref.Host = host
		ref.APIHost = host
	}

	if !validRepositoryPath(remainder) {
		return NormalizedRef{}, ErrUnsupportedReference
	}
	ref.Path = remainder

	if slash := strings.LastIndex(remainder, "/"); slash >= 0 {
		ref.Namespace = remainder[:slash]
		ref.Name = remainder[slash+1:]
	} else {
		ref.Name = remainder
	}

	ref.Canonical = ref.Host + "/" + ref.Path + referenceSuffix(ref.Tag, ref.Digest)
	ref.Familiar = familiarForm(ref)
	return ref, nil
}

// UnsupportedReferenceKey is the stored identity of a reference that
// NormalizeImageRef refused.
//
// A refused reference has no canonical form, so the only identity available is
// the raw string the container declared -- bounded here, because it comes from
// a container's configuration and is therefore untrusted. Nothing is parsed,
// lowercased or otherwise interpreted: this is an opaque key, not a reference.
//
// It exists so the two places that must agree on that key cannot drift. Image
// intelligence WRITES an unsupported record under it, and the planner READS the
// record back under it. They used to compute it separately and the planner's
// half was missing entirely, so a container whose reference could not be
// normalised was silently absent from planning rather than reported as
// unassessable.
//
// Returns "" for an empty reference, which names no record.
func UnsupportedReferenceKey(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > MaxReferenceBytes {
		return raw[:MaxReferenceBytes]
	}
	return raw
}

// splitRegistryHost separates a leading registry host from the repository path.
//
// Docker's rule, and the reason it is a rule rather than a guess: the first
// component is a host only when it looks like one -- it contains a dot or a
// colon, or it is exactly "localhost". Without that, "ubuntu/nginx" would be
// read as the host "ubuntu".
func splitRegistryHost(repository string) (host, remainder string) {
	slash := strings.Index(repository, "/")
	if slash < 0 {
		return "", repository
	}

	candidate := repository[:slash]
	if strings.ContainsAny(candidate, ".:") || candidate == "localhost" {
		return candidate, repository[slash+1:]
	}
	return "", repository
}

// ContactableRegistryHost reports whether a host may become an HTTPS
// destination.
//
// Exported because one URL in the registry client does NOT come from an image
// reference: the bearer-token realm, which a registry supplies in its
// WWW-Authenticate challenge. That host has to clear exactly the same bar as a
// reference's, and sharing this function is what guarantees it does rather than
// hoping two checks stay in step.
func ContactableRegistryHost(host string) bool { return contactableHost(host) }

// contactableHost reports whether a registry host may become an HTTPS
// destination.
//
// This is the SSRF gate, and it is intentionally narrow. See NormalizeImageRef
// for the reasoning behind each refusal. A second, independent check runs at
// dial time against the RESOLVED ADDRESS -- see internal/registry -- because a
// name that passes here can still resolve into private space, and DNS can
// change between the two moments.
func contactableHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	// A port means a non-default endpoint, which in practice means a local or
	// internal registry. Refused rather than contacted.
	if strings.ContainsAny(host, ":@/?#\\ ") {
		return false
	}
	// An IP literal is refused outright: no public registry requires one, and a
	// reference naming an address rather than a name is indistinguishable from
	// an attempt to steer HarborMaster at an internal service.
	if net.ParseIP(host) != nil {
		return false
	}
	if host == "localhost" || strings.EqualFold(host, "localhost.") {
		return false
	}
	// A dot is required, which is what excludes single-label internal names
	// like "registry" that resolve only inside a cluster.
	if !strings.Contains(host, ".") {
		return false
	}
	// A trailing dot is a valid FQDN but is a needless second spelling of the
	// same host, which would fragment the cache and the health table.
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") ||
		strings.Contains(host, "..") {
		return false
	}

	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if !isHostLabel(label) {
			return false
		}
	}
	return true
}

// isHostLabel reports whether a DNS label contains only letters, digits and
// hyphens, and does not begin or end with a hyphen.
//
// An allowlist, so anything that could carry meaning to a URL parser -- an
// underscore, a percent escape, a colon -- is refused rather than escaped.
func isHostLabel(label string) bool {
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// validRepositoryPath reports whether a repository path is safe to place in a
// URL path.
//
// An allowlist of the characters the distribution specification permits. Notably
// absent: '.' sequences that could traverse, '%' which could smuggle an encoded
// separator, and anything that terminates a path.
func validRepositoryPath(path string) bool {
	if path == "" || len(path) > MaxRepositoryBytes {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") ||
		strings.Contains(path, "//") {
		return false
	}

	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
				r == '.', r == '_', r == '-':
			default:
				// Uppercase is deliberately refused: the distribution
				// specification requires lowercase repository paths, and
				// accepting both spellings would create two cache entries for
				// one repository.
				return false
			}
		}
	}
	return true
}

// ValidImageTag reports whether a tag is well formed.
//
// Exported because tag names also arrive FROM a registry, in a tag listing, and
// a registry is untrusted input. A tag that fails this check is discarded
// rather than stored or displayed: it would otherwise reach a URL, a database
// column, and a UI.
func ValidImageTag(tag string) bool { return validTag(tag) }

// ValidImageDigest reports whether a digest is well formed.
//
// Exported for the same reason as ValidImageTag: digests arrive from registry
// responses as well as from local inventory.
func ValidImageDigest(digest string) bool { return validDigest(digest) }

// validTag reports whether a tag is safe to place in a URL path.
//
// The distribution specification's own rule: up to 128 characters of word
// characters, dots and hyphens, not beginning with a dot or a hyphen.
func validTag(tag string) bool {
	if tag == "" || len(tag) > MaxTagBytes {
		return false
	}
	if strings.HasPrefix(tag, ".") || strings.HasPrefix(tag, "-") {
		return false
	}
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// validDigest reports whether a digest is well formed.
//
// Only sha256 and sha512 are accepted, with an exact hex length. A digest
// becomes part of a URL path, so a loose check here would be a path-injection
// gap; requiring an exact shape closes it completely.
func validDigest(digest string) bool {
	algorithm, hex, found := strings.Cut(digest, ":")
	if !found {
		return false
	}

	var want int
	switch algorithm {
	case "sha256":
		want = 64
	case "sha512":
		want = 128
	default:
		return false
	}
	if len(hex) != want {
		return false
	}
	for _, r := range hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// referenceSuffix renders the tag and digest part of a canonical reference.
func referenceSuffix(tag, digest string) string {
	suffix := ""
	if tag != "" {
		suffix += ":" + tag
	}
	if digest != "" {
		suffix += "@" + digest
	}
	return suffix
}

// familiarForm renders the short reference an operator recognises.
//
// The inverse of normalization: the Docker Hub host and the library namespace
// are implied rather than shown, because "nginx:1.25" is what someone typed and
// "docker.io/library/nginx:1.25" is what it means.
func familiarForm(ref NormalizedRef) string {
	path := ref.Path
	if ref.Kind == RegistryDockerHub {
		path = strings.TrimPrefix(path, DockerHubLibraryNamespace+"/")
		return path + referenceSuffix(ref.Tag, ref.Digest)
	}
	return ref.Host + "/" + path + referenceSuffix(ref.Tag, ref.Digest)
}
