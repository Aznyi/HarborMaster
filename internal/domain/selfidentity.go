package domain

import (
	"strings"
)

// Recognising HarborMaster's own container.
//
// # Why this exists
//
// Phase 11 gave HarborMaster the ability to update a container without being
// asked. Applied to itself, that is not an update — it is a process stopping
// its own container partway through an operation it is in the middle of:
//
//	stop the original  ->  the process dies here  ->  nothing else runs
//
// No checkpoint is written, no verification happens, no rollback is possible,
// and the record says "creating" forever. Whether the replacement came up is
// then a question nobody can answer from HarborMaster's own records, because
// the thing that writes those records is the thing that was stopped.
//
// So it is refused. Not deferred, not made safe — refused.
//
// # Detection is layered, and no single signal is trusted
//
// A container cannot reliably ask Docker "which one am I". The daemon does not
// tell it, `/proc` says different things on cgroup v1 and v2, and a hostname is
// whatever the operator set. Any one of those can be absent or wrong.
//
// So SelfIdentity carries several INDEPENDENT signals and SelfMatch asks
// whether ANY of them fires. Each is sufficient; none is required. A deployment
// where the runtime probe fails still refuses on the image, and one where the
// image was retagged still refuses on the label.
//
// # What happens when nothing matches
//
// Automation proceeds. That is the deliberate choice, and it is worth being
// explicit about why "fail closed" here does not mean "refuse everything": a
// detection failure that blocked the whole estate would make the feature
// unusable on any host where a probe did not work, and operators would turn the
// protection off. Instead the exclusion is REPORTED — the API and UI say which
// container HarborMaster believes is itself, and say so plainly when it does
// not know — so a wrong answer is visible rather than silent.

// SelfIdentity is what HarborMaster knows about its own container.
//
// Every field is optional. A field that could not be established is empty, and
// an empty field never matches anything — a blank container id must not
// accidentally exclude a container whose id is also unknown.
type SelfIdentity struct {
	// ContainerID is the full 64-character id of HarborMaster's own container,
	// when it could be determined. The strongest signal: an id is unique, is
	// not chosen by an operator, and cannot be confused with another container.
	ContainerID string `json:"containerId,omitempty"`

	// ContainerName is the name that id resolved to in the inventory. Carried
	// for display and for the pause and policy paths, which are keyed on names
	// because ids change on recreation.
	ContainerName string `json:"containerName,omitempty"`

	// ImageRef is the image reference HarborMaster is running, and ImageID its
	// resolved local id. A second, independent signal: a container running the
	// same image as HarborMaster is HarborMaster, or is a second copy of it,
	// and updating either from inside the first is the same mistake.
	ImageRef string `json:"imageRef,omitempty"`
	ImageID  string `json:"imageId,omitempty"`

	// Source names how the container id was established, in operator-facing
	// words, so a wrong answer is diagnosable rather than mysterious.
	Source SelfIdentitySource `json:"source,omitempty"`

	// Detail is HarborMaster's own sentence about the detection, including the
	// case where it failed. Never a raw error and never a file path.
	Detail string `json:"detail,omitempty"`
}

// SelfIdentitySource names how the running container was recognised.
type SelfIdentitySource string

// The detection sources, strongest first.
const (
	// SelfSourceConfigured means an operator named the container explicitly.
	// Trusted above every probe: somebody who knows is better than a guess.
	SelfSourceConfigured SelfIdentitySource = "configured"
	// SelfSourceRuntime means the id was read from the process's own control
	// group or mount information.
	SelfSourceRuntime SelfIdentitySource = "runtime"
	// SelfSourceHostname means the hostname matched a container's short id,
	// which is the daemon's default when no hostname is set.
	SelfSourceHostname SelfIdentitySource = "hostname"
	// SelfSourceLabel means a container carried the self-identifying label.
	SelfSourceLabel SelfIdentitySource = "label"
	// SelfSourceNone means HarborMaster could not determine which container it
	// is running in — or is not running in one at all.
	SelfSourceNone SelfIdentitySource = "unknown"
)

// LabelSelfIdentity is the label a deployment may set on HarborMaster's own
// container to identify it beyond doubt.
//
// The supported deployments set it. It is a BACKSTOP rather than the primary
// signal, because a label is operator-supplied and this is the one place where
// an operator's mistake would be silent — a container mislabelled as
// HarborMaster is simply never updated, which is safe, and HarborMaster
// mislabelled as something else still matches on its id and its image.
const LabelSelfIdentity = "io.harbormaster.self"

// Known reports whether HarborMaster established which container it is.
func (s SelfIdentity) Known() bool {
	return s.ContainerID != "" || s.ImageRef != "" || s.ImageID != ""
}

// Describe renders the identity for an operator.
func (s SelfIdentity) Describe() string {
	switch {
	case s.ContainerName != "":
		return "HarborMaster is running as the container " + s.ContainerName
	case s.ContainerID != "":
		return "HarborMaster is running as the container " + ShortenID(s.ContainerID)
	case s.ImageRef != "":
		return "HarborMaster is running the image " + s.ImageRef +
			", but could not determine which container is its own"
	default:
		return "HarborMaster could not determine which container it is running in"
	}
}

// SelfMatch reports whether a container is HarborMaster itself.
//
// # Any signal is enough
//
// The checks are independent on purpose. An estate where the runtime probe
// failed still refuses on the image; one where the image was retagged still
// refuses on the id; one where both are unknown still refuses on the label.
//
// # An empty signal matches nothing
//
// Every comparison requires BOTH sides to be non-empty. Without that, a
// SelfIdentity whose ContainerID could not be determined would match every
// container whose id was also blank, which on a malformed inventory row would
// silently exclude the estate.
func (s SelfIdentity) SelfMatch(target SelfTarget) (bool, string) {
	// 1. The id. Unique, not operator-chosen, and unambiguous.
	if s.ContainerID != "" && target.ContainerID != "" &&
		strings.EqualFold(s.ContainerID, target.ContainerID) {
		return true, "this is the container HarborMaster is running in"
	}

	// 2. The name, when the id is known but the caller only has a name. Pauses
	// and policy selectors are keyed on names, so this is the form those paths
	// can compare.
	if s.ContainerName != "" && target.ContainerName != "" &&
		strings.EqualFold(s.ContainerName, target.ContainerName) {
		return true, "this is the container HarborMaster is running in"
	}

	// 3. The image id. Independent of any container identity: two containers
	// from one image are two copies of HarborMaster, and updating either from
	// inside one of them is the same mistake.
	if s.ImageID != "" && target.ImageID != "" &&
		strings.EqualFold(s.ImageID, target.ImageID) {
		return true, "this container runs the same image as HarborMaster itself"
	}

	// 4. The image reference, compared on repository rather than on tag. A
	// HarborMaster at :0.9 and one at :0.10 are both HarborMaster.
	if s.ImageRef != "" && target.ImageRef != "" &&
		sameRepository(s.ImageRef, target.ImageRef) {
		return true, "this container runs the same image as HarborMaster itself"
	}

	// 5. The label. Last, because it is the only operator-supplied signal, and
	// present at all because it is the one that keeps working when a deployment
	// does something none of the probes above anticipated.
	if value, ok := target.Labels[LabelSelfIdentity]; ok && truthyLabel(value) {
		return true, "this container is labelled as HarborMaster itself"
	}

	return false, ""
}

// SelfTarget is the container being tested, as much of it as a caller has.
//
// A projection rather than a whole container: the comparison must not be able
// to reach configuration it has no business reading, and a narrow input keeps
// SelfMatch pure and cheap to test.
type SelfTarget struct {
	ContainerID   string
	ContainerName string
	ImageRef      string
	ImageID       string
	Labels        map[string]string
}

// sameRepository compares two references by repository, ignoring the tag and
// the digest.
//
// `ghcr.io/aznyi/harbormaster:0.9` and `ghcr.io/aznyi/harbormaster:0.10` are the
// same software at two versions, and neither may update the other. A digest
// reference to the same repository is the same software too.
func sameRepository(left, right string) bool {
	l, r := repositoryOf(left), repositoryOf(right)
	return l != "" && l == r
}

// repositoryOf strips the tag or digest from a reference.
//
// Deliberately simple: this is a comparison between two references
// HarborMaster read from its own inventory, not a parser for hostile input.
// domain.ParseImageRef is the real parser; this exists so a comparison does not
// need to allocate one.
func repositoryOf(reference string) string {
	trimmed := strings.TrimSpace(strings.ToLower(reference))
	if trimmed == "" {
		return ""
	}
	// A digest reference: everything before the '@'.
	if at := strings.Index(trimmed, "@"); at >= 0 {
		trimmed = trimmed[:at]
	}
	// A tag: the last ':' AFTER the last '/', so a registry port is not
	// mistaken for a tag separator.
	slash := strings.LastIndex(trimmed, "/")
	if colon := strings.LastIndex(trimmed, ":"); colon > slash {
		trimmed = trimmed[:colon]
	}
	return trimmed
}

// truthyLabel reads a boolean label without pulling strconv into the hot path.
//
// Generous in what it accepts, because the failure direction is safe: a label
// this does not recognise means "not HarborMaster", and every other signal
// still applies.
func truthyLabel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
