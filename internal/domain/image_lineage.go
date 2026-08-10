package domain

import (
	"strings"
	"time"
)

// Image lineage: the difference between what a container RUNS and what
// HarborMaster FOLLOWS.
//
// # The bug this exists to close
//
// A recreation is digest-pinned on purpose. The replacement is created from
// `repo@sha256:...` -- the exact immutable artefact that was planned, acquired,
// approved and verified -- and never from the mutable tag it came from, because
// a tag can move between the approval and the create.
//
// That is correct, and on its own it was also fatal. The next inventory pass saw
// only `repo@sha256:...`; image intelligence answered, accurately, that a digest
// cannot move; the planner had nothing to propose; and the container fell out of
// automation permanently. Every container received exactly one automated update.
//
// The fix is not to recreate from a tag. It is to stop conflating two different
// references that were previously the same string:
//
//	EXECUTION REFERENCE  repo@sha256:...   what the container runs. Immutable.
//	TRACKING REFERENCE   repo:tag          what HarborMaster watches for news.
//
// Update discovery resolves the TRACKING reference against the registry and
// compares the digest it yields against the digest actually RUNNING. Execution
// still only ever uses a digest that came out of the existing pipeline.
//
// # Why the database is authoritative and the label is not
//
// Lineage is HarborMaster's own claim about a container it manages, so it lives
// in HarborMaster's own store. The container also carries a label saying what it
// tracks, which makes lineage recoverable and diagnosable from Docker alone --
// but a label is written by whoever created the container, and anyone can set
// one. A label is therefore EVIDENCE, never authority: it can corroborate a
// record HarborMaster already holds, and it can be offered to a reconciliation
// that validates it, but it can never by itself make HarborMaster believe it
// manages something.

// LineageState is how much HarborMaster knows about what a container follows.
type LineageState string

const (
	// LineageTracked means a tracking reference is known and trusted, so update
	// discovery can run for this container.
	LineageTracked LineageState = "tracked"
	// LineageUntracked means the container runs a digest HarborMaster cannot
	// attribute to any tag it trusts. Recorded explicitly rather than left
	// absent: "we looked and there is nothing to follow" is a different fact
	// from "we have not looked", and an operator needs to be able to see it.
	//
	// An untracked container is not a broken one. A container someone pinned to
	// a digest by hand is CORRECTLY untracked, and inventing a tag for it would
	// be HarborMaster guessing what an operator meant.
	LineageUntracked LineageState = "untracked"
)

// ValidLineageState reports whether value names a state.
func ValidLineageState(value string) bool {
	return value == string(LineageTracked) || value == string(LineageUntracked)
}

// LineageOrigin records what evidence established the lineage.
//
// Kept because the origins are not equally strong, and a reconciliation that
// has to choose between two claims should be able to see which one HarborMaster
// watched happen.
type LineageOrigin string

const (
	// LineageObserved means the container declared a tag and HarborMaster wrote
	// down what it saw. The ordinary case for a container someone deployed
	// normally.
	LineageObserved LineageOrigin = "observed"
	// LineageRecreated means HarborMaster performed the recreation itself and
	// carried the tracking reference across it. The strongest evidence there
	// is: HarborMaster chose the digest and knows which tag it came from.
	LineageRecreated LineageOrigin = "recreation"
	// LineageMigrated means lineage was recovered from HarborMaster's own
	// historical records for a container an earlier version had already
	// digest-pinned.
	LineageMigrated LineageOrigin = "migration"
)

// ValidLineageOrigin reports whether value names an origin.
func ValidLineageOrigin(value string) bool {
	switch LineageOrigin(value) {
	case LineageObserved, LineageRecreated, LineageMigrated:
		return true
	}
	return false
}

// ImageLineage is what HarborMaster follows for one managed container.
//
// Keyed by container NAME rather than id, and deliberately: a recreation
// replaces the container and its id with it, so an id-keyed record would stop
// describing the container the moment it did its job. The rest of the automation
// surface -- update policies, pauses, approvals -- already keys by name for the
// same reason.
type ImageLineage struct {
	// ContainerName is the identity. Unique.
	ContainerName string `json:"containerName"`
	// ContainerID is the container observed at the last write. EVIDENCE, not
	// identity: it is how a reconciliation notices that something replaced the
	// container without HarborMaster's involvement.
	ContainerID string `json:"containerId,omitempty"`

	State  LineageState  `json:"state"`
	Origin LineageOrigin `json:"origin"`

	// TrackingReference is the canonical mutable reference update discovery
	// resolves, e.g. "docker.io/library/nginx:1.27". Empty when untracked.
	TrackingReference string `json:"trackingReference,omitempty"`
	// TrackingFamiliar is the short form an operator recognises.
	TrackingFamiliar string `json:"trackingFamiliar,omitempty"`
	// Repository is the canonical repository path of the tracking reference.
	// Held separately so a cross-repository substitution can be refused without
	// re-parsing, and so a mismatch is visible in the record itself.
	Repository string `json:"repository,omitempty"`

	// RunningDigest is the digest HarborMaster believes is executing. Advanced
	// only by a verified recreation or a completed rollback -- never by a pull,
	// and never by observation alone.
	RunningDigest string `json:"runningDigest,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Tracked reports whether update discovery may run for this container.
func (l ImageLineage) Tracked() bool {
	return l.State == LineageTracked && l.TrackingReference != "" && l.Repository != ""
}

// TrackingReferenceTag returns the tag segment of the tracking reference.
//
// The tag alone, for the plan's CurrentTag field: a reader needs to see WHICH
// tag is being followed alongside the digest that is running, and the two must
// stay separately legible rather than being collapsed into one string.
func (l ImageLineage) TrackingReferenceTag() string {
	reference := l.TrackingReference
	slash := strings.LastIndex(reference, "/")
	colon := strings.LastIndex(reference, ":")
	if colon <= slash {
		return ""
	}
	return reference[colon+1:]
}

// LineageLabel is the container label carrying the tracking reference.
//
// Namespaced under the same `io.harbormaster.` prefix as every other label
// HarborMaster reads or writes, and under `image.` because it describes the
// image lineage rather than the update policy (`io.harbormaster.update.*`) or
// the process identity (`io.harbormaster.self`).
//
// WRITTEN by a recreation, so lineage survives a lost database and can be read
// by a person with nothing but `docker inspect`. NEVER trusted on its own --
// see LineageFromLabel.
const LineageLabel = "io.harbormaster.image.tracking"

// MaxLineageReferenceBytes bounds a label-supplied reference before it is
// parsed. The label is attacker-controlled on any container HarborMaster did
// not create, so it is bounded before it reaches the parser rather than after.
const MaxLineageReferenceBytes = MaxReferenceBytes

// LineageRefusal says why a lineage claim was not accepted.
//
// A fixed vocabulary, in HarborMaster's own words. None of these is assembled
// from a label's contents, because the label is untrusted input and echoing it
// into an explanation is how untrusted input reaches a log and a UI.
type LineageRefusal string

const (
	// LineageRefusalNone means the claim was accepted.
	LineageRefusalNone LineageRefusal = ""
	// LineageRefusalAbsent means there was no claim to evaluate.
	LineageRefusalAbsent LineageRefusal = "absent"
	// LineageRefusalMalformed means the reference did not parse, was too long,
	// or named no registry repository.
	LineageRefusalMalformed LineageRefusal = "malformed"
	// LineageRefusalNotTag means the reference carried a digest rather than a
	// tag. A tracking reference has to be able to move; a digest cannot.
	LineageRefusalNotTag LineageRefusal = "notATag"
	// LineageRefusalRepositoryMismatch means the claimed tracking reference
	// belongs to a different repository than the image actually running. This
	// is the cross-repository substitution guard: accepting it would let a
	// label point HarborMaster's update discovery at an attacker's repository.
	LineageRefusalRepositoryMismatch LineageRefusal = "repositoryMismatch"
	// LineageRefusalUncorroborated means a label claimed lineage for a
	// container HarborMaster has no record of managing. A label alone must
	// never confer managed status.
	LineageRefusalUncorroborated LineageRefusal = "uncorroborated"
)

// Explain renders the refusal for an operator.
func (r LineageRefusal) Explain() string {
	switch r {
	case LineageRefusalAbsent:
		return "no tracking reference was supplied"
	case LineageRefusalMalformed:
		return "the tracking reference is not a reference HarborMaster can parse"
	case LineageRefusalNotTag:
		return "the tracking reference names a digest; a reference that is followed for updates has to be a tag"
	case LineageRefusalRepositoryMismatch:
		return "the tracking reference names a different repository than the image this container is running"
	case LineageRefusalUncorroborated:
		return "the container carries a tracking label but HarborMaster has no record of managing it"
	default:
		return ""
	}
}

// EstablishLineage decides what HarborMaster may claim about a container it has
// just observed, given no prior record.
//
// The rule is narrow on purpose:
//
//   - A container running a TAG is tracked, following that tag. This is what an
//     operator meant by deploying `repo:1.27`.
//   - A container running a DIGEST is untracked. HarborMaster does not know
//     which tag, if any, that digest came from, and every way of guessing --
//     asking the registry which tags resolve here, assuming `latest`, reading
//     the repository's conventions -- produces a tag the operator never chose.
//
// runningDigest may be empty; a tag-referenced container whose digest could not
// be resolved is still tracked, because the tag is what discovery needs. The
// digest is filled in by the first successful check.
func EstablishLineage(
	containerName, containerID string,
	observed NormalizedRef,
	runningDigest string,
	now time.Time,
) ImageLineage {
	lineage := ImageLineage{
		ContainerName: containerName,
		ContainerID:   containerID,
		State:         LineageUntracked,
		Origin:        LineageObserved,
		RunningDigest: strings.TrimSpace(runningDigest),
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}

	// A digest-pinned reference, or one that named no repository at all (an
	// image built on this host), carries nothing to follow.
	if observed.Tag == "" || observed.Digest != "" || observed.Path == "" {
		if observed.Digest != "" && lineage.RunningDigest == "" {
			lineage.RunningDigest = observed.Digest
		}
		return lineage
	}

	lineage.State = LineageTracked
	lineage.TrackingReference = observed.Canonical
	lineage.TrackingFamiliar = observed.Familiar
	lineage.Repository = observed.Path
	return lineage
}

// LineageFromLabel validates a tracking reference a CONTAINER claimed.
//
// # Why this is the most hostile input in the feature
//
// Anyone who can run `docker run -l io.harbormaster.image.tracking=...` can put
// a string here, including on a container HarborMaster has never touched. If
// that string were believed, an unprivileged user on the host could:
//
//   - make HarborMaster treat their container as managed, and
//   - point its update discovery at a repository they control, and
//   - have HarborMaster propose, acquire and run an image from it.
//
// So the label is checked against things the label cannot influence. It must
// parse, it must name a tag rather than a digest, and it must belong to the SAME
// REPOSITORY as the image the container is actually running. The last one is the
// substitution guard: a label can say which TAG of the running repository to
// follow, and can never say which repository.
//
// `corroborated` is the caller's answer to the separate question of whether
// HarborMaster has its own record of managing this container. False refuses the
// claim outright, whatever the label says. A label is evidence for a record that
// exists; it is not a way to create one.
func LineageFromLabel(
	label string,
	running NormalizedRef,
	corroborated bool,
) (NormalizedRef, LineageRefusal) {
	claim := strings.TrimSpace(label)
	if claim == "" {
		return NormalizedRef{}, LineageRefusalAbsent
	}
	if len(claim) > MaxLineageReferenceBytes {
		return NormalizedRef{}, LineageRefusalMalformed
	}
	if !corroborated {
		return NormalizedRef{}, LineageRefusalUncorroborated
	}

	parsed, err := NormalizeImageRef(claim)
	if err != nil || parsed.Path == "" {
		return NormalizedRef{}, LineageRefusalMalformed
	}
	if parsed.Digest != "" || parsed.Tag == "" {
		return NormalizedRef{}, LineageRefusalNotTag
	}
	// The repository the container is RUNNING is the anchor. A claim about any
	// other repository is refused rather than reconciled.
	if running.Path == "" || !strings.EqualFold(parsed.Path, running.Path) ||
		!strings.EqualFold(parsed.Host, running.Host) {
		return NormalizedRef{}, LineageRefusalRepositoryMismatch
	}
	return parsed, LineageRefusalNone
}

// LineageProposal is the per-container update verdict lineage makes possible.
//
// The shared image-intelligence record answers a question about a REFERENCE:
// "has this tag moved, and is there a newer one?". This answers the question
// about a CONTAINER: "is what this container is running still what its tag
// resolves to?". They differ precisely when a container has been moved onto a
// digest, which after Phase 13.1 is every container HarborMaster has updated.
type LineageProposal struct {
	// Update is the size of the change, or UpdateNone.
	Update UpdateType
	// Reference and Digest name what to move onto. Both empty unless Update is
	// an actual update.
	Reference string
	Familiar  string
	Digest    string
	// Reason explains the verdict in HarborMaster's own words.
	Reason string
	// Usable reports that the verdict rests on evidence good enough to plan
	// from. False means "not established", never "no update".
	Usable bool
}

// EvaluateLineage decides whether a tracked container has an update waiting.
//
// The comparison the whole change exists to make:
//
//	tracking reference -> registry digest   VS   digest actually running
//
// Ordering matters. A newer TAG is preferred over a moved digest, because
// moving `1.27 -> 1.28` is the change the operator's policy ceiling is written
// about, and reporting it as a bare digest move would let a `digestOnly` policy
// apply a minor upgrade.
//
// Fails closed everywhere. An unusable verdict is never UpdateNone: "we could
// not establish anything" and "there is nothing to do" are the two answers this
// codebase refuses to collapse.
func EvaluateLineage(lineage ImageLineage, intel ImageIntel, runningDigest string) LineageProposal {
	if !lineage.Tracked() {
		return LineageProposal{Update: UpdateNone, Usable: false,
			Reason: "this container has no tracking reference, so nothing says what it should follow"}
	}
	// The intelligence handed in must be the intelligence for the reference
	// this container follows. A caller that looked up the wrong row would
	// otherwise compare against another repository's registry answer.
	if !strings.EqualFold(intel.Reference, lineage.TrackingReference) {
		return LineageProposal{Update: UpdateUnknown, Usable: false,
			Reason: "the registry evidence does not belong to this container's tracking reference"}
	}
	if !strings.EqualFold(intel.Repository, lineage.Repository) {
		return LineageProposal{Update: UpdateUnknown, Usable: false,
			Reason: "the registry evidence names a different repository than this container's tracking reference"}
	}
	if intel.Status != CheckOK {
		return LineageProposal{Update: UpdateUnknown, Usable: false,
			Reason: "the registry information for the tracking reference is missing or did not complete"}
	}

	running := strings.TrimSpace(runningDigest)
	if running == "" {
		running = strings.TrimSpace(lineage.RunningDigest)
	}
	if running == "" {
		return LineageProposal{Update: UpdateUnknown, Usable: false,
			Reason: "the digest this container is running could not be established, so there is nothing to compare"}
	}

	// A newer tag in the same series, when the check found one and resolved it.
	// Both halves are required: a tag with no digest cannot be acquired, and
	// pairing a tag with a digest resolved for a different one is the mistake
	// the acquisition path refuses anyway.
	if intel.LatestTag != "" && intel.LatestDigest != "" &&
		intel.Update != UpdateNone && intel.Update != UpdateUnknown {
		if strings.EqualFold(intel.LatestDigest, running) {
			// The newer tag already resolves to what is running. Moving would
			// change the reference and nothing else.
			return LineageProposal{Update: UpdateNone, Usable: true,
				Reason: "the newest tag already resolves to the digest this container is running"}
		}
		reference, familiar := retagged(lineage, intel.LatestTag)
		return LineageProposal{
			Update:    intel.Update,
			Reference: reference,
			Familiar:  familiar,
			Digest:    intel.LatestDigest,
			Usable:    true,
			Reason:    "a newer tag is published in the same series",
		}
	}

	// The tag itself moved: same reference, different content.
	if intel.RemoteDigest != "" && !strings.EqualFold(intel.RemoteDigest, running) {
		return LineageProposal{
			Update:    UpdateDigest,
			Reference: lineage.TrackingReference,
			Familiar:  lineage.TrackingFamiliar,
			Digest:    intel.RemoteDigest,
			Usable:    true,
			Reason:    "the tracking tag now resolves to a different digest than this container is running",
		}
	}
	if intel.RemoteDigest == "" {
		return LineageProposal{Update: UpdateUnknown, Usable: false,
			Reason: "the registry did not report a digest for the tracking reference"}
	}
	return LineageProposal{Update: UpdateNone, Usable: true,
		Reason: "the tracking tag resolves to the digest this container is already running"}
}

// retagged renders lineage's repository under a different tag.
//
// Built from the LINEAGE's own canonical reference rather than from anything the
// registry returned, so a hostile tag listing cannot rewrite the repository. Only
// the tag segment is replaced.
func retagged(lineage ImageLineage, tag string) (canonical, familiar string) {
	canonical = replaceTag(lineage.TrackingReference, tag)
	familiar = replaceTag(lineage.TrackingFamiliar, tag)
	if familiar == "" {
		familiar = canonical
	}
	return canonical, familiar
}

// replaceTag swaps the tag segment of a reference, leaving everything before it
// untouched. Returns an empty string when the input carries no tag to replace.
func replaceTag(reference, tag string) string {
	if reference == "" || tag == "" {
		return ""
	}
	// The tag is the last colon-separated segment, but only when that colon
	// comes after the final slash -- otherwise it is a registry port.
	slash := strings.LastIndex(reference, "/")
	colon := strings.LastIndex(reference, ":")
	if colon <= slash {
		return ""
	}
	return reference[:colon+1] + tag
}

// AdvanceLineage carries lineage across a completed recreation.
//
// Called only after verification has passed. The tracking reference becomes the
// APPROVED reference's tag -- which for a moved tag is the same tag, and for a
// series upgrade is the newer one. Both come from the plan that was approved, so
// neither is guessed.
//
// A proposed reference that is not a usable tracking reference leaves the
// tracking reference alone rather than clearing it: the container has moved onto
// a new digest of something HarborMaster was already following, and forgetting
// what it follows would reintroduce the bug this file exists to close.
func AdvanceLineage(
	previous ImageLineage,
	approvedReference NormalizedRef,
	approvedDigest string,
	containerID string,
	now time.Time,
) ImageLineage {
	next := previous
	next.ContainerID = containerID
	next.Origin = LineageRecreated
	next.UpdatedAt = now.UTC()
	if digest := strings.TrimSpace(approvedDigest); digest != "" {
		next.RunningDigest = digest
	}
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now.UTC()
	}

	if approvedReference.Tag != "" && approvedReference.Digest == "" && approvedReference.Path != "" {
		// Only ever within the repository already being followed, or when
		// nothing was being followed yet.
		if previous.Repository == "" || strings.EqualFold(previous.Repository, approvedReference.Path) {
			next.State = LineageTracked
			next.TrackingReference = approvedReference.Canonical
			next.TrackingFamiliar = approvedReference.Familiar
			next.Repository = approvedReference.Path
		}
	}
	return next
}

// RestoreLineage returns lineage to the digest a rollback put back.
//
// The tracking reference is untouched -- a rollback undoes which ARTEFACT is
// running, not which tag the operator asked HarborMaster to follow. Leaving
// lineage claiming the replacement's digest would have automation compare
// against a digest that is not running, and conclude there was nothing to do.
func RestoreLineage(
	previous ImageLineage,
	restoredDigest string,
	containerID string,
	now time.Time,
) ImageLineage {
	next := previous
	next.UpdatedAt = now.UTC()
	if containerID != "" {
		next.ContainerID = containerID
	}
	if digest := strings.TrimSpace(restoredDigest); digest != "" {
		next.RunningDigest = digest
	}
	return next
}
