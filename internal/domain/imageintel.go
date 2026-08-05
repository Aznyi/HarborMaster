package domain

import "time"

// Image intelligence: what HarborMaster knows about an image beyond what the
// local daemon reports.
//
// # It reads registries; it never changes anything
//
// The engine behind this model resolves manifests and lists tags over HTTPS. It
// does not pull, push, delete, prune, tag, or recreate anything, and there is no
// call into docker.Runtime anywhere behind it. An update is REPORTED; applying
// one is an operator's job with their own tooling.
//
// # It never carries a credential
//
// Every registry request is anonymous. HarborMaster reads no Docker config, no
// keychain, and no credential helper, and accepts none through configuration.
// A private repository therefore reports "unauthorized" -- an honest statement
// that the answer is unavailable, rather than a reason to start handling
// secrets.

// CheckStatus is the outcome of the most recent registry lookup.
//
// The distinctions matter to an operator and to the retry logic: two of these
// mean "try again later", and three mean "there is nothing to retry".
type CheckStatus string

// Check statuses.
const (
	// CheckPending means the image has never been looked up.
	CheckPending CheckStatus = "pending"
	// CheckOK means the most recent lookup succeeded.
	CheckOK CheckStatus = "ok"
	// CheckFailed means a transient failure -- a timeout, a 5xx, a connection
	// reset. Retried with backoff.
	CheckFailed CheckStatus = "failed"
	// CheckRateLimited means the registry asked HarborMaster to slow down.
	// Retried, but on the registry's schedule rather than HarborMaster's.
	CheckRateLimited CheckStatus = "rateLimited"
	// CheckUnauthorized means the repository is private. HarborMaster holds no
	// credentials by design, so this is a permanent answer rather than a
	// failure, and it is not retried aggressively.
	CheckUnauthorized CheckStatus = "unauthorized"
	// CheckNotFound means the repository or tag does not exist upstream. Often
	// a locally built image that was never published.
	CheckNotFound CheckStatus = "notFound"
	// CheckUnsupported means the reference cannot safely become a request: a
	// local registry, an address literal, or a malformed reference. Nothing to
	// retry, and deliberately distinguished from a failure so a dashboard does
	// not show a healthy estate as broken.
	CheckUnsupported CheckStatus = "unsupported"
)

// CheckStatuses lists every status.
var CheckStatuses = []CheckStatus{
	CheckPending, CheckOK, CheckFailed, CheckRateLimited,
	CheckUnauthorized, CheckNotFound, CheckUnsupported,
}

// ValidCheckStatus reports whether name is a known status.
func ValidCheckStatus(name string) bool {
	for _, status := range CheckStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// Retryable reports whether another attempt could produce a different answer.
//
// Unauthorized is retryable in the weak sense that a repository can become
// public, but it is not a fault and is retried on the slow schedule rather than
// the failure backoff.
func (s CheckStatus) Retryable() bool {
	return s == CheckFailed || s == CheckRateLimited || s == CheckPending
}

// Terminal reports whether the status means there is nothing to retry.
func (s CheckStatus) Terminal() bool { return s == CheckUnsupported }

// Platform is the OS/architecture a manifest targets.
//
// Recorded because a multi-architecture image resolves to a DIFFERENT digest
// per platform, so "the digest changed" is only meaningful once the platform is
// fixed. HarborMaster asks for the platform the local image reports, which is
// the one the container is actually running.
type Platform struct {
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
}

// String renders the platform in the conventional os/arch[/variant] form.
func (p Platform) String() string {
	if p.OS == "" && p.Architecture == "" {
		return ""
	}
	rendered := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		rendered += "/" + p.Variant
	}
	return rendered
}

// Empty reports whether no platform was determined.
func (p Platform) Empty() bool { return p.OS == "" && p.Architecture == "" }

// ImageIntel is everything known about one image reference.
//
// One row per REFERENCE rather than per container: a hundred containers running
// the same image are one registry lookup, which is the difference between a
// polite client and a rate-limited one.
type ImageIntel struct {
	ID int64 `json:"id"`

	// Reference is the canonical form and the identity of this record.
	// Familiar is the short form an operator recognises.
	Reference string `json:"reference"`
	Familiar  string `json:"familiar"`

	Kind      RegistryKind `json:"registryKind"`
	Registry  string       `json:"registry"`
	Namespace string       `json:"namespace,omitempty"`
	// Repository is the full path within the registry, e.g. "library/nginx".
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`

	// LocalDigest is the manifest digest the local daemon reports for this
	// reference, and RemoteDigest the one the registry currently serves. The
	// pair is the whole of digest-based update detection.
	LocalDigest  string `json:"localDigest,omitempty"`
	RemoteDigest string `json:"remoteDigest,omitempty"`
	// Pinned reports that the reference names a digest, so its tag cannot move.
	Pinned bool `json:"pinned"`

	Platform Platform `json:"platform,omitempty"`

	// ImageID links to the local image row, when one exists. A reference can
	// have intelligence without a local image: a container may name an image
	// the daemon has since removed.
	ImageID string `json:"imageId,omitempty"`
	// ContainerCount is how many present containers use this reference.
	ContainerCount int `json:"containerCount"`

	Update UpdateType `json:"updateType"`
	// LatestTag is the newer tag found by version comparison, when there was
	// one. Empty for a digest-only update.
	LatestTag string `json:"latestTag,omitempty"`
	// UpdateReason explains the verdict, from a fixed set of phrases.
	UpdateReason string `json:"updateReason,omitempty"`

	Status CheckStatus `json:"checkStatus"`
	// StatusDetail explains a non-OK status in HarborMaster's own words. Never
	// a registry-supplied string: see the package comment in internal/registry.
	StatusDetail string `json:"statusDetail,omitempty"`

	// FirstSeenAt is when the reference entered the inventory, LastCheckedAt
	// the most recent lookup attempt, and LastSuccessAt the most recent one
	// that answered.
	FirstSeenAt   time.Time  `json:"firstSeenAt"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	// NextCheckAt is when the scheduler will consider this reference again. It
	// is what implements both the refresh interval and the failure backoff.
	NextCheckAt *time.Time `json:"nextCheckAt,omitempty"`
	// FailureCount drives the backoff and is reset by any success.
	FailureCount int `json:"failureCount"`

	// PublishedAt is when the remote image was built, from the manifest's
	// config or an OCI annotation. Frequently absent.
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	// Vendor and Source come from OCI annotations. Bounded and sanitised
	// before storage.
	Vendor string `json:"vendor,omitempty"`
	Source string `json:"source,omitempty"`
	// Labels holds the OCI annotations that were kept. A bounded allowlist of
	// keys, not everything the registry sent.
	Labels map[string]string `json:"labels,omitempty"`

	// ETag is the conditional-request validator for this reference's manifest.
	//
	// NEVER SERIALISED. It is a cache implementation detail with no meaning to
	// an API consumer, and it is registry-supplied text: keeping it out of the
	// response is one less place it could be rendered.
	ETag string `json:"-"`
}

// ETagForRequest returns the cached validator, or empty when it is unusable.
//
// Checked on the way OUT as well as on the way in. The value round-trips
// through the database, and a header assembled from a stored string is a
// header-injection gap if the string was never constrained -- a restored backup
// or a hand-edited row would be enough.
func (i ImageIntel) ETagForRequest() string {
	if i.ETag == "" || len(i.ETag) > 256 {
		return ""
	}
	for _, r := range i.ETag {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return i.ETag
}

// UpdateAvailable reports whether an actionable update was found.
func (i ImageIntel) UpdateAvailable() bool {
	return i.Update.Available()
}

// ImageUpdateEvent is one observed CHANGE for a reference.
//
// Append-only, and written only when something actually moved: a digest
// changed, an update appeared or was taken, or the check status changed. Writing
// a row per check would grow the table with every refresh interval and record
// nothing, which is the opposite of a history.
type ImageUpdateEvent struct {
	ID int64 `json:"id"`
	// Reference identifies the image this event belongs to.
	Reference string `json:"reference"`

	ObservedAt time.Time      `json:"observedAt"`
	Kind       ImageEventKind `json:"kind"`

	// PreviousDigest and CurrentDigest bound a digest change. Both empty for an
	// event that is not about digests.
	PreviousDigest string `json:"previousDigest,omitempty"`
	CurrentDigest  string `json:"currentDigest,omitempty"`

	// PreviousUpdate and CurrentUpdate bound a change in update availability.
	PreviousUpdate UpdateType `json:"previousUpdateType,omitempty"`
	CurrentUpdate  UpdateType `json:"currentUpdateType,omitempty"`
	LatestTag      string     `json:"latestTag,omitempty"`

	// Status is the check status at the time of the event.
	Status CheckStatus `json:"checkStatus"`
	// Detail is HarborMaster's own description of the event, from a fixed set
	// of phrases.
	Detail string `json:"detail,omitempty"`
}

// ImageEventKind names what changed.
type ImageEventKind string

// Image event kinds.
const (
	// ImageEventDiscovered is the first successful lookup for a reference.
	ImageEventDiscovered ImageEventKind = "discovered"
	// ImageEventDigestChanged means the remote digest for the same reference
	// moved. For a mutable tag this is the publisher republishing it.
	ImageEventDigestChanged ImageEventKind = "digestChanged"
	// ImageEventUpdateFound means an update became available, or its type
	// changed.
	ImageEventUpdateFound ImageEventKind = "updateFound"
	// ImageEventUpdateCleared means a previously reported update is gone --
	// usually because the container was moved onto the newer image.
	ImageEventUpdateCleared ImageEventKind = "updateCleared"
	// ImageEventCheckFailed means the lookup started failing.
	ImageEventCheckFailed ImageEventKind = "checkFailed"
	// ImageEventCheckRecovered means it started working again.
	ImageEventCheckRecovered ImageEventKind = "checkRecovered"
)

// ImageEventKinds lists every kind.
var ImageEventKinds = []ImageEventKind{
	ImageEventDiscovered, ImageEventDigestChanged,
	ImageEventUpdateFound, ImageEventUpdateCleared,
	ImageEventCheckFailed, ImageEventCheckRecovered,
}

// ValidImageEventKind reports whether name is a known kind.
func ValidImageEventKind(name string) bool {
	for _, kind := range ImageEventKinds {
		if string(kind) == name {
			return true
		}
	}
	return false
}

// RegistryHealth is one registry host's recent behaviour.
//
// Kept per HOST rather than per image because rate limits, outages, and
// backoff are properties of the endpoint. It is also what lets the UI say "the
// updates are stale because Docker Hub is rate-limiting us" rather than showing
// a hundred individually failed images with no explanation.
type RegistryHealth struct {
	Host string       `json:"host"`
	Kind RegistryKind `json:"registryKind"`

	// Images is how many references this host serves.
	Images int `json:"images"`

	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureAt *time.Time `json:"lastFailureAt,omitempty"`
	// ConsecutiveFailures drives the per-host backoff, and is reset by any
	// success.
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// AvailableAt is when the host may be contacted again. Set by the backoff
	// and by a registry's own Retry-After.
	AvailableAt *time.Time `json:"availableAt,omitempty"`

	// LastDetail is HarborMaster's description of the most recent failure.
	// Never a registry-supplied string.
	LastDetail string `json:"lastDetail,omitempty"`
	// RateLimited reports that the most recent failure was a rate limit, which
	// an operator should read very differently from an outage.
	RateLimited bool `json:"rateLimited"`
}

// Healthy reports whether the host is currently answering.
func (h RegistryHealth) Healthy() bool { return h.ConsecutiveFailures == 0 }

// ImageIntelSummary is the dashboard aggregate.
//
// Computed by grouped aggregate queries rather than by counting a list: the
// summary is what a dashboard polls, so it must stay cheap on an estate with
// ten thousand references.
type ImageIntelSummary struct {
	// Images counts distinct references tracked, and Containers how many
	// present containers they cover.
	Images     int `json:"images"`
	Containers int `json:"containers"`

	// UpdatesAvailable counts references with an actionable update, and
	// ContainersAffected the containers running them -- the number an operator
	// actually plans around.
	UpdatesAvailable   int `json:"updatesAvailable"`
	ContainersAffected int `json:"containersAffected"`

	ByUpdate   map[UpdateType]int  `json:"byUpdateType"`
	ByStatus   map[CheckStatus]int `json:"byCheckStatus"`
	ByRegistry map[string]int      `json:"byRegistry"`

	// Checked counts references that have been looked up at least once. The
	// difference between this and Images is the coverage a dashboard must not
	// hide: an estate where nothing has been checked is not an estate with no
	// updates.
	Checked int `json:"checked"`
	// Pending counts references awaiting a first lookup.
	Pending int `json:"pending"`
	// Unsupported counts references that will never be looked up, so a
	// dashboard can explain a permanent gap in coverage.
	Unsupported int `json:"unsupported"`

	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	// Registries carries per-host health, so the UI can attribute staleness.
	Registries []RegistryHealth `json:"registries,omitempty"`
}

// Coverage returns the share of tracked references that have been checked, in
// the range 0..1.
//
// Reported so a UI can say "412 of 500 checked" rather than presenting an
// update count as though it covered the estate.
func (s ImageIntelSummary) Coverage() float64 {
	if s.Images <= 0 {
		return 0
	}
	return float64(s.Checked) / float64(s.Images)
}

// ImageIntelEngineStatus reports the scheduler's state.
type ImageIntelEngineStatus struct {
	Enabled bool `json:"enabled"`
	// DueNow is how many references are past their next-check time.
	DueNow int `json:"dueNow"`
	// Running reports that a collection pass is in flight.
	Running bool `json:"running"`
	// SweepPending reports that a pass is owed.
	SweepPending bool `json:"sweepPending"`
	// LastSweepAt is when the most recent pass finished.
	LastSweepAt *time.Time `json:"lastSweepAt,omitempty"`
	// Checked, Skipped and Failed describe that pass.
	Checked int `json:"lastChecked"`
	Skipped int `json:"lastSkipped"`
	Failed  int `json:"lastFailed"`
}
