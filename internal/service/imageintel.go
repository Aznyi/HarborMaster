package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The image intelligence engine.
//
// # What it does, in one sentence
//
// It projects every image reference in the inventory into a canonical form,
// asks the registry what that reference resolves to now, and records whether an
// update exists.
//
// # What it deliberately does NOT do
//
// It never pulls, pushes, deletes, prunes, tags, or recreates anything, and
// there is no call into docker.Runtime anywhere below. It reads the inventory
// HarborMaster has already persisted and it reads registries over HTTPS. An
// update is REPORTED; applying one is an operator's job with their own tooling.
//
// It holds no credentials. Every registry request is anonymous, and a private
// repository is reported as "unauthorized" rather than becoming a reason to
// start handling secrets.
//
// # The properties that matter
//
// A FAILED LOOKUP NEVER OVERWRITES A GOOD ANSWER. If the registry is
// unreachable, the previous digest and verdict remain the best knowledge
// available. Blanking them would turn "we could not ask" into "no update is
// available", which is a different and false claim.
//
// AN INCOMPLETE TAG LISTING IS "UNKNOWN", NOT "NONE". A listing that stopped at
// its budget has not established that no newer tag exists.
//
// WORK IS BOUNDED EVERYWHERE. Concurrent requests, references per pass,
// retries, response sizes, and per-host backoff all have caps, because the peer
// on the other end is a third party that HarborMaster must stay welcome at.

// ErrImageIntelDisabled reports that image intelligence is switched off.
var ErrImageIntelDisabled = errors.New("image intelligence is disabled")

// ImageIntelStore is the persistence capability the engine needs.
//
// A narrow interface rather than *store.ImageIntelRepository, so the engine is
// testable without a database and so the surface it depends on is visible in
// one place.
type ImageIntelStore interface {
	InventoryReferences(ctx context.Context, limit int) ([]store.InventoryReference, error)
	SyncReferences(ctx context.Context, seeds []store.ImageReferenceSeed, now time.Time) (store.SyncResult, error)
	Due(ctx context.Context, now time.Time, limit int, unavailableHosts []string) ([]domain.ImageIntel, error)
	CountDue(ctx context.Context, now time.Time) (int, error)
	Get(ctx context.Context, reference string) (domain.ImageIntel, error)
	RecordCheck(ctx context.Context, outcome store.CheckOutcome, now time.Time) error
	RecordHostOutcome(ctx context.Context, outcome store.HostOutcome, now time.Time) error
	UnavailableHosts(ctx context.Context, now time.Time) ([]string, error)
	HostFailureCount(ctx context.Context, host string) (int, error)
	PruneHistory(ctx context.Context, cutoff time.Time, batch int) (int64, error)
	PruneOrphans(ctx context.Context, batch int) (int64, error)
}

// RegistryClient is the registry capability the engine needs.
//
// Narrow on purpose, and worth reading for what is absent: there is no method
// that writes to a registry, and no way to supply a URL. A caller can ask about
// a reference, and that is all.
type RegistryClient interface {
	Manifest(ctx context.Context, req registry.ManifestRequest) (registry.ManifestResult, error)
	Tags(ctx context.Context, ref domain.NormalizedRef, maxPages int) (registry.TagsResult, error)
}

// ImageIntelOptions configures an ImageIntelService.
type ImageIntelOptions struct {
	Store    ImageIntelStore
	Registry RegistryClient

	// Lineage supplies the TRACKING references of managed containers. Nil
	// restores the pre-Phase-13.1 behaviour, in which only the references
	// containers declare are resolved -- and a container HarborMaster has
	// updated declares an immutable digest, so it would never be checked again.
	Lineage LineageReader
	// Reconciler establishes and corrects lineage against the observed estate.
	// Runs at the start of each pass, before the projection that reads from it.
	Reconciler *LineageReconciler

	Config config.ImageIntel
	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	Notify Notifier

	Logger *slog.Logger
	Now    func() time.Time
}

// ImageIntelService discovers and records image update information.
type ImageIntelService struct {
	store    ImageIntelStore
	registry RegistryClient
	// lineage supplies the TRACKING references of managed containers, which are
	// what has to be resolved for a container that runs a digest. Nil restores
	// the pre-Phase-13.1 behaviour: only declared references are checked.
	lineage LineageReader
	// reconciler keeps lineage honest against what the inventory observed.
	reconciler *LineageReconciler

	cfg      config.ImageIntel
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time

	// wake carries a one-slot signal for an out-of-band collection request.
	// Capacity 1 with a non-blocking send: a second request while one is unread
	// is redundant, and dropping it is what stops a producer ever blocking.
	wake chan struct{}

	// collecting guards the collection pass so two cannot overlap. Two passes
	// would double the load on a third-party registry, which is the one
	// resource HarborMaster does not own.
	collecting sync.Mutex

	// state guards the status fields, which HTTP handlers read while the worker
	// writes them.
	state  sync.RWMutex
	status collectionState
}

// collectionState is the mutable part of the engine's status.
type collectionState struct {
	running      bool
	sweepPending bool
	lastSweepAt  *time.Time
	checked      int
	skipped      int
	failed       int
}

// NewImageIntelService builds an ImageIntelService.
func NewImageIntelService(opts ImageIntelOptions) *ImageIntelService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.MaxConcurrentRequests < 1 {
		cfg.MaxConcurrentRequests = config.DefaultImageIntelMaxConcurrentRequests
	}
	if cfg.MaxReferencesPerPass < 1 {
		cfg.MaxReferencesPerPass = config.DefaultImageIntelMaxReferencesPerPass
	}
	if cfg.MaxTrackedReferences < 1 {
		cfg.MaxTrackedReferences = config.DefaultImageIntelMaxTrackedReferences
	}
	if cfg.MaxTagPages < 1 {
		cfg.MaxTagPages = config.DefaultImageIntelMaxTagPages
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = config.DefaultImageIntelRefreshInterval
	}
	if cfg.FailureBackoff <= 0 {
		cfg.FailureBackoff = config.DefaultImageIntelFailureBackoff
	}
	if cfg.MaxFailureBackoff <= 0 {
		cfg.MaxFailureBackoff = config.DefaultImageIntelMaxFailureBackoff
	}
	if cfg.UnsupportedInterval <= 0 {
		cfg.UnsupportedInterval = config.DefaultImageIntelUnsupportedInterval
	}

	return &ImageIntelService{
		store:      opts.Store,
		registry:   opts.Registry,
		lineage:    opts.Lineage,
		reconciler: opts.Reconciler,
		cfg:        cfg,
		notifier:   opts.Notify,
		logger:     logger,
		now:        now,
		wake:       make(chan struct{}, 1),
	}
}

// Enabled reports whether image intelligence is switched on.
func (s *ImageIntelService) Enabled() bool { return s.cfg.Enabled }

// ready reports whether the engine is configured well enough to run.
func (s *ImageIntelService) ready() bool {
	return s.cfg.Enabled && s.store != nil && s.registry != nil
}

// ------------------------------------------------------------------ sync --

// SyncInventory projects the inventory's image references into tracked records.
//
// ONE query reads every distinct reference the estate uses, and one transaction
// writes them. A reference used by a hundred containers is one row and one
// future registry lookup, which is the difference between a client a registry
// tolerates and one it blocks.
//
// A reference that cannot safely become a request -- a local registry, an
// address literal, a malformed reference -- is still TRACKED, marked
// unsupported. Dropping it would leave a silent gap in coverage; recording it
// lets the dashboard say why the gap exists.
func (s *ImageIntelService) SyncInventory(ctx context.Context) (store.SyncResult, error) {
	if !s.ready() {
		return store.SyncResult{}, ErrImageIntelDisabled
	}

	references, err := s.store.InventoryReferences(ctx, s.cfg.MaxTrackedReferences)
	if err != nil {
		return store.SyncResult{}, err
	}

	seeds := make([]store.ImageReferenceSeed, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		seed := s.seed(reference)
		seeds = append(seeds, seed)
		seen[strings.ToLower(seed.Reference)] = struct{}{}
	}

	// THE TRACKING REFERENCES.
	//
	// A container HarborMaster has updated runs `repo@sha256:...`, and the
	// loop above therefore seeds a digest reference -- which is immutable, and
	// answering "has this moved?" for it is answering the wrong question. What
	// has to be resolved against the registry is the TAG that container
	// follows, so those are seeded here from the authoritative lineage record.
	//
	// Without this the estate is checked exactly as it was before Phase 13.1
	// and every updated container reports `none` forever.
	seeds = append(seeds, s.lineageSeeds(ctx, seen, len(seeds))...)

	if len(seeds) == 0 {
		return store.SyncResult{}, nil
	}

	return s.store.SyncReferences(ctx, seeds, s.now().UTC())
}

// lineageSeeds adds the tracking reference of every tracked container that the
// inventory sweep did not already cover.
//
// Never fails the sync. Lineage is an enhancement to coverage: without it the
// estate is still checked, just as it was before, so an unreadable lineage
// table degrades to the previous behaviour rather than stopping update
// discovery for everything.
func (s *ImageIntelService) lineageSeeds(
	ctx context.Context,
	seen map[string]struct{},
	already int,
) []store.ImageReferenceSeed {
	if s.lineage == nil {
		return nil
	}
	tracked, err := s.lineage.Tracked(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "image lineage could not be read; update discovery is covering "+
			"only the references containers declare",
			slog.String("error", err.Error()))
		return nil
	}

	budget := s.cfg.MaxTrackedReferences - already
	seeds := make([]store.ImageReferenceSeed, 0, len(tracked))
	for _, lineage := range tracked {
		if budget <= 0 {
			s.logger.WarnContext(ctx, "the tracked reference limit was reached before every "+
				"managed container's tracking reference was covered",
				slog.Int("limit", s.cfg.MaxTrackedReferences))
			break
		}
		key := strings.ToLower(lineage.TrackingReference)
		if _, duplicate := seen[key]; duplicate {
			continue
		}

		// Re-parsed rather than trusted: this string is read back out of the
		// database and is about to become a registry request, so it is
		// validated at the point of use like any other stored reference.
		normalized, parseErr := domain.NormalizeImageRef(lineage.TrackingReference)
		if parseErr != nil || normalized.Tag == "" || normalized.Digest != "" {
			continue
		}

		seen[key] = struct{}{}
		budget--
		seeds = append(seeds, store.ImageReferenceSeed{
			Reference:  normalized.Canonical,
			Familiar:   normalized.Familiar,
			Kind:       normalized.Kind,
			Registry:   normalized.Host,
			Namespace:  normalized.Namespace,
			Repository: normalized.Path,
			Tag:        normalized.Tag,
			// No local digest and no image id, deliberately. The local daemon
			// may not carry this tag at all -- it was pulled by digest -- and
			// the comparison this seed exists for is made per container in the
			// planner, against the digest lineage says is running, not against
			// whatever the local tag happens to point at.
			LocalDigestDetail: domain.RepoDigestNone.Explain(),
			Pinned:            false,
			Supported:         true,
		})
	}
	return seeds
}

// seed projects one inventory reference into a tracked record.
func (s *ImageIntelService) seed(reference store.InventoryReference) store.ImageReferenceSeed {
	platform := domain.Platform{
		OS:           reference.OS,
		Architecture: reference.Architecture,
		Variant:      reference.Variant,
	}

	normalized, err := domain.NormalizeImageRef(reference.Reference)
	if err != nil {
		// Tracked but never contacted. The canonical form is unavailable, so
		// the raw reference is the identity -- bounded, because an unbounded one
		// would not have reached the inventory either.
		//
		// Computed by the SHARED helper rather than here, because the planner
		// reads this record back under the same key and the two deriving it
		// separately is exactly how the planner came to miss it.
		raw := domain.UnsupportedReferenceKey(reference.Reference)
		return store.ImageReferenceSeed{
			Reference: raw,
			Familiar:  raw,
			Kind:      domain.RegistryUnknown,
			ImageID:   reference.ImageID,
			Platform:  platform,
			Supported: false,
			Detail:    unsupportedDetail,
		}
	}

	// The digest of the image this container is actually running.
	//
	// Two sources, in order of directness:
	//
	//  1. The REFERENCE itself, when the container was started from a
	//     digest-pinned one. Unambiguous: the container names the content.
	//  2. The local image's RepoDigests, for the ordinary tag-referenced case.
	//     Only an entry naming THIS registry and repository is eligible, and
	//     anything ambiguous resolves to empty rather than to a guess -- see
	//     domain.SelectRepoDigest.
	//
	// Validated rather than trusted in both cases: this value reaches a
	// comparison, a change plan, and a UI, and a malformed one would be none of
	// those.
	local := reference.Digest
	if local != "" && !domain.ValidImageDigest(local) {
		local = ""
	}
	digestSource := domain.RepoDigestResolved
	if local == "" {
		local, digestSource = domain.SelectRepoDigest(reference.RepoDigests, normalized)
	}

	return store.ImageReferenceSeed{
		Reference:   normalized.Canonical,
		Familiar:    normalized.Familiar,
		Kind:        normalized.Kind,
		Registry:    normalized.Host,
		Namespace:   normalized.Namespace,
		Repository:  normalized.Path,
		Tag:         normalized.Tag,
		LocalDigest: local,
		// Why the local digest is missing, when it is. Carried so the change
		// plan can say "this image was built here" rather than the bare
		// "no registry digest" that used to be the only available phrasing.
		LocalDigestDetail: digestSource.Explain(),
		ImageID:           reference.ImageID,
		ContainerCount:    reference.ContainerCount,
		Pinned:            normalized.Pinned(),
		Platform:          platform,
		Supported:         true,
	}
}

// unsupportedDetail explains a reference that will never be looked up.
//
// HarborMaster's own words, matching the phrasing internal/registry uses for
// the same condition.
const unsupportedDetail = "the image reference cannot be looked up: it names no public registry"

// --------------------------------------------------------------- checking --

// CheckResult reports what one collection pass did.
type CheckResult struct {
	Checked int
	Skipped int
	Failed  int
}

// Collect performs one bounded collection pass.
//
// # How the work is bounded
//
//   - The batch is capped by MaxReferencesPerPass, so a large estate is covered
//     over several passes rather than all at once.
//   - Concurrency is capped by MaxConcurrentRequests, so a registry sees a
//     handful of connections rather than one per image.
//   - Hosts currently backing off are excluded IN SQL, so a rate-limited
//     registry does not fill the batch with work that would be skipped.
//
// Two passes never overlap: the second is refused rather than queued, because
// duplicating load on a third party is the one cost HarborMaster cannot absorb.
func (s *ImageIntelService) Collect(ctx context.Context) (CheckResult, error) {
	var result CheckResult
	if !s.ready() {
		return result, ErrImageIntelDisabled
	}

	if !s.collecting.TryLock() {
		return result, nil
	}
	defer s.collecting.Unlock()

	s.setRunning(true)
	defer s.setRunning(false)

	now := s.now().UTC()
	unavailable, err := s.store.UnavailableHosts(ctx, now)
	if err != nil {
		return result, err
	}

	due, err := s.store.Due(ctx, now, s.cfg.MaxReferencesPerPass, unavailable)
	if err != nil {
		return result, err
	}
	if len(due) == 0 {
		return result, nil
	}

	// A bounded worker pool. The semaphore is the concurrency limit, and it is
	// held by the CALLER rather than by the registry client so the number is
	// visible where the politeness decision is made.
	var (
		wait      sync.WaitGroup
		semaphore = make(chan struct{}, s.cfg.MaxConcurrentRequests)
		mu        sync.Mutex
	)

	for _, record := range due {
		if ctx.Err() != nil {
			break
		}

		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wait.Wait()
			return result, ctx.Err()
		}

		wait.Add(1)
		go func(record domain.ImageIntel) {
			defer wait.Done()
			defer func() { <-semaphore }()

			outcome := s.check(ctx, record)

			// Persistence is serialised behind the mutex because SQLite has one
			// writer. Doing it inside the worker would just queue at the
			// database while holding a registry slot.
			mu.Lock()
			defer mu.Unlock()

			switch outcome.Status {
			case domain.CheckOK:
				result.Checked++
			case domain.CheckUnsupported:
				result.Skipped++
			default:
				result.Failed++
			}

			if err := s.persist(ctx, record, outcome); err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("could not record image check",
						slog.String("reference", record.Familiar),
						slog.String("error", err.Error()))
				}
			}
		}(record)
	}

	wait.Wait()
	return result, nil
}

// check performs one reference's registry lookup and classifies the outcome.
//
// Returns a fully formed outcome even on failure: the caller persists it either
// way, because "we tried and it failed" is a fact the scheduler needs.
func (s *ImageIntelService) check(ctx context.Context, record domain.ImageIntel) store.CheckOutcome {
	outcome := store.CheckOutcome{Reference: record.Reference}

	normalized, err := domain.NormalizeImageRef(record.Reference)
	if err != nil {
		outcome.Status = domain.CheckUnsupported
		outcome.Detail = unsupportedDetail
		outcome.NextCheckAt = s.now().Add(s.cfg.UnsupportedInterval)
		return outcome
	}

	manifest, err := s.registry.Manifest(ctx, registry.ManifestRequest{
		Ref:  normalized,
		ETag: record.ETagForRequest(),
	})
	if err != nil {
		return s.failure(ctx, record, normalized, err)
	}

	outcome.Status = domain.CheckOK
	outcome.ETag = manifest.ETag

	// A 304 means the manifest is byte-identical to the cached one, so the
	// digest cannot have moved. The previous digest is carried forward rather
	// than blanked.
	remoteDigest := record.RemoteDigest
	if !manifest.NotModified {
		remoteDigest = manifest.Digest
		outcome.Platform, outcome.PlatformMissing = pickPlatform(record.Platform, manifest.Platforms)
		outcome.PublishedAt = parsePublished(manifest.Annotations)
		outcome.Vendor = manifest.Annotations["vendor"]
		outcome.Source = manifest.Annotations["source"]
		outcome.Labels = manifest.Annotations
	}
	outcome.RemoteDigest = remoteDigest

	assessment := s.assess(ctx, record, normalized, remoteDigest)
	outcome.Update = assessment.Type
	outcome.LatestTag = assessment.Tag
	outcome.UpdateReason = assessment.Reason

	// A newer TAG is only actionable with its OWN digest.
	//
	// The tag came from a listing; the digest has to come from that tag's
	// manifest. Resolving it here, from the same check and the same registry
	// evidence, is what lets the planner propose the pair together instead of
	// pairing a new tag with the current tag's digest.
	if assessment.Tag != "" {
		digest, resolveErr := s.resolveTagDigest(ctx, normalized, assessment.Tag)
		if resolveErr == nil {
			outcome.LatestDigest = digest
		} else {
			// The tag exists but cannot be pinned. Reported as an update whose
			// size is unknown rather than as a proposal: acquisition is
			// digest-pinned, so an unpinnable tag is not something an operator
			// can be offered.
			outcome.LatestTag = ""
			outcome.Update = domain.UpdateUnknown
			outcome.UpdateReason = "a newer tag was published but its digest could not be " +
				"resolved, so the change cannot be pinned"
			s.logger.WarnContext(ctx, "a newer tag could not be resolved to a digest",
				slog.String("reference", record.Reference),
				slog.String("latestTag", assessment.Tag))
		}
	}

	outcome.NextCheckAt = s.nextCheck(0)
	return outcome
}

// resolveTagDigest reads the manifest digest of one specific tag.
//
// The reference is rebuilt from the NORMALIZED current one with only the tag
// replaced, so the registry, repository, and platform are carried over
// unchanged and the lookup cannot be redirected somewhere else by the tag
// listing's contents. The tag itself is re-validated by NormalizeImageRef
// before it reaches a URL.
func (s *ImageIntelService) resolveTagDigest(
	ctx context.Context,
	current domain.NormalizedRef,
	tag string,
) (string, error) {
	target, err := domain.NormalizeImageRef(current.Host + "/" + current.Path + ":" + tag)
	if err != nil {
		return "", err
	}
	// Belt and braces: normalization must not have moved the repository.
	if target.Host != current.Host || target.Path != current.Path {
		return "", domain.ErrTargetCrossed
	}

	manifest, err := s.registry.Manifest(ctx, registry.ManifestRequest{Ref: target})
	if err != nil {
		return "", err
	}
	if manifest.Digest == "" || !domain.ValidImageDigest(manifest.Digest) {
		return "", domain.ErrTargetDigestInvalid
	}
	return manifest.Digest, nil
}

// assess decides what update, if any, is available.
//
// # Two questions, not one
//
// A check asks the registry two things, and they are independent:
//
//  1. Does the EXACT CONFIGURED REFERENCE still resolve to the digest this
//     container runs? Answered by one manifest HEAD, always, before this
//     function is called -- the answer arrives as remoteDigest.
//  2. Does a NEWER VERSION TAG exist anywhere in the repository? Answered by a
//     bounded tag enumeration, and only when the configured tag is a version.
//
// The first is cheap and always answerable. The second is bounded by a page
// budget and is frequently UNANSWERABLE on a large repository: `library/nginx`
// publishes far more tags than the configured budget can carry.
//
// # The rule, and the defect it was written for
//
//	Bounded version discovery may be incomplete. That must not erase a
//	positively established fact about the exact configured reference.
//
// Until Phase 17.5 an unanswerable second question discarded a positively
// answered first one: a truncated listing returned `unknown`, and `unknown`
// short-circuited before the digest comparison ran. No strategy permits
// `unknown`, so a container on a versioned tag whose digest had genuinely moved
// was never updated -- permanently, because a repository never gets smaller.
//
// Nothing here fetches anything new. The manifest HEAD has already happened and
// remoteDigest is already in hand; the fix is that an answer HarborMaster
// already had stops being thrown away.
//
// # The precedence
//
//	D. A CONCRETE NEWER TAG outranks same-tag digest movement, because it is the
//	   more actionable of two true answers. Unchanged.
//	E. Discovery truncated, nothing newer seen: report the digest move if there
//	   was one, and SAY that discovery was incomplete. Otherwise `unknown`.
//	F. Discovery complete, nothing newer: report the digest move if there was
//	   one, otherwise `none`.
//
// Truncation may never manufacture `none`, and may never manufacture a version
// update that was not observed.
func (s *ImageIntelService) assess(
	ctx context.Context,
	record domain.ImageIntel,
	normalized domain.NormalizedRef,
	remoteDigest string,
) domain.UpdateAssessment {
	// B. The fact about the configured reference. Established first and held,
	// so no later branch can reach a conclusion without having considered it.
	moved := digestMoved(record.LocalDigest, remoteDigest)

	// C. Version discovery, when the reference admits it. A digest-pinned
	// reference cannot have "moved" -- it names immutable content -- and a
	// non-version tag has no series to search.
	var (
		discovery domain.UpdateAssessment
		searched  bool
		truncated bool
	)
	if !normalized.Pinned() && normalized.Tag != "" {
		if _, versioned := domain.ParseTagVersion(normalized.Tag); versioned {
			discovery, truncated, searched = s.compareTags(ctx, normalized)
		}
	}

	// D. A concrete newer tag was OBSERVED.
	//
	// Keyed on the TAG rather than on the type, deliberately. A calendar-versioned
	// candidate is reported as `unknown` with a tag -- HarborMaster saw a newer
	// tag but will not assert what size of change it represents -- and that is a
	// different thing from having seen nothing at all. Keying on the type would
	// fold the two together and lose the tag.
	if searched && discovery.Tag != "" {
		return discovery
	}

	// E and F. Nothing newer was observed, either because there is nothing or
	// because the search could not finish.
	if moved {
		assessment := domain.UpdateAssessment{
			Type: domain.UpdateDigest,
			Reason: "the registry serves a different digest for this tag; " +
				"the publisher has republished it",
		}
		if truncated {
			// Said out loud rather than folded away. The digest movement is
			// positively known; whether a newer VERSION tag also exists is not,
			// and an operator reading "digest update" deserves to know which of
			// the two questions went unanswered.
			assessment.Reason += ". Version discovery also reached the configured " +
				"registry search limit, so a newer version tag may exist beyond " +
				"what was read"
		}
		return assessment
	}

	if truncated {
		// No movement, and the search could not finish. HarborMaster genuinely
		// does not know, and must not say `none`.
		return domain.UpdateAssessment{
			Type: domain.UpdateUnknown,
			Reason: "HarborMaster could not finish version discovery within the " +
				"configured registry search limit, so it cannot determine whether " +
				"a newer version tag exists",
		}
	}

	if record.LocalDigest == "" {
		// Nothing to compare against. A locally built image, or one whose
		// digest the daemon never recorded.
		return domain.UpdateAssessment{
			Type: domain.UpdateUnknown,
			Reason: "the local image has no registry digest to compare, so only " +
				"a newer tag could be detected",
		}
	}
	return domain.UpdateAssessment{
		Type:   domain.UpdateNone,
		Reason: "the tag resolves to the digest already in use",
	}
}

// compareTags enumerates the repository's tags and classifies the newest.
//
// Returns the classification, whether the listing was TRUNCATED, and whether it
// ran at all. Truncation is returned separately rather than inferred from the
// verdict: "unknown because the budget ran out" and "unknown because the
// candidate is calendar-versioned" are different facts, and assess has to tell
// them apart to know whether it may fall back to the digest comparison.
//
// Reports ok=false when tag listing is unavailable, which is a normal condition
// on a registry that does not implement it rather than a failure.
func (s *ImageIntelService) compareTags(
	ctx context.Context,
	normalized domain.NormalizedRef,
) (assessment domain.UpdateAssessment, truncated, ok bool) {
	// The SAME bounded call as before. MaxTagPages, the per-page cap, the
	// tracked-tag cap, and the response-size cap are all unchanged: this change
	// is control flow over evidence already gathered, not more evidence.
	tags, err := s.registry.Tags(ctx, normalized, s.cfg.MaxTagPages)
	if err != nil {
		// A listing failure does not fail the whole check: the digest
		// comparison is still available and still true.
		return domain.UpdateAssessment{}, false, false
	}
	return domain.ClassifyTagUpdate(normalized.Tag, tags.Tags, tags.Truncated),
		tags.Truncated, true
}

// digestMoved reports whether two digests differ meaningfully.
//
// A missing digest on either side is NOT a difference: it is an absence of
// evidence, and reporting it as an update would flag every locally built image
// as out of date.
func digestMoved(local, remote string) bool {
	if local == "" || remote == "" {
		return false
	}
	return !strings.EqualFold(local, remote)
}

// failure builds the outcome for a lookup that did not answer.
//
// Nothing the registry established is written: see the package comment. The
// classification and the detail come from internal/registry, which maps a
// failure to HarborMaster's own words rather than the registry's.
func (s *ImageIntelService) failure(
	ctx context.Context,
	record domain.ImageIntel,
	normalized domain.NormalizedRef,
	err error,
) store.CheckOutcome {
	status, detail := registry.Classify(err)

	outcome := store.CheckOutcome{
		Reference:    record.Reference,
		Status:       status,
		Detail:       detail,
		FailureCount: record.FailureCount + 1,
	}

	switch status {
	case domain.CheckUnsupported:
		// Nothing to retry. Re-examined rarely, so a reference that becomes
		// supported is eventually picked up without being polled.
		outcome.FailureCount = 0
		outcome.NextCheckAt = s.now().Add(s.cfg.UnsupportedInterval)

	case domain.CheckRateLimited:
		// The registry's own guidance is honoured. A client that ignores
		// Retry-After earns a longer ban.
		wait := registry.RetryAfterFor(err)
		if wait.Set && wait.Wait > 0 {
			outcome.NextCheckAt = s.now().Add(wait.Wait)
		} else {
			outcome.NextCheckAt = s.nextCheckAfterFailure(outcome.FailureCount)
		}

	case domain.CheckUnauthorized, domain.CheckNotFound:
		// A permanent answer rather than a fault. Re-checked on the slow
		// schedule, because a repository can become public and an image can be
		// published later.
		outcome.FailureCount = 0
		outcome.NextCheckAt = s.now().Add(s.cfg.UnsupportedInterval)

	default:
		outcome.NextCheckAt = s.nextCheckAfterFailure(outcome.FailureCount)
	}

	// The host's health is recorded separately, so one registry's outage backs
	// off every reference it serves rather than each discovering it alone.
	s.recordHost(ctx, normalized, status, detail, err)
	return outcome
}

// recordHost updates a registry host's health after a lookup.
//
// Best effort and deliberately detached from the check's own error handling: a
// health row that could not be written must not fail the check that produced
// it.
func (s *ImageIntelService) recordHost(
	ctx context.Context,
	normalized domain.NormalizedRef,
	status domain.CheckStatus,
	detail string,
	err error,
) {
	host := normalized.Host
	if host == "" {
		return
	}

	// Only conditions that are the HOST's fault back the host off. A private
	// repository or a missing tag says nothing about the registry's health, and
	// treating it as an outage would stop checking every other image there.
	hostFault := status == domain.CheckFailed || status == domain.CheckRateLimited
	if !hostFault {
		s.hostSuccess(ctx, host, normalized.Kind)
		return
	}

	// WithoutCancel keeps the caller's values while dropping its cancellation:
	// a health record must survive the request that discovered it failing, and
	// the timeout is what stops it outliving that request by much.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostWriteTimeout)
	defer cancel()

	failures, readErr := s.store.HostFailureCount(writeCtx, host)
	if readErr != nil {
		failures = 0
	}
	failures++

	available := s.nextCheckAfterFailure(failures)
	if wait := registry.RetryAfterFor(err); wait.Set && wait.Wait > 0 {
		available = s.now().Add(wait.Wait)
	}

	if writeErr := s.store.RecordHostOutcome(writeCtx, store.HostOutcome{
		Host:                host,
		Kind:                normalized.Kind,
		Success:             false,
		Detail:              detail,
		RateLimited:         status == domain.CheckRateLimited,
		AvailableAt:         available,
		ConsecutiveFailures: failures,
	}, s.now().UTC()); writeErr != nil {
		s.logger.Debug("could not record registry host failure",
			slog.String("host", host), slog.String("error", writeErr.Error()))
	}

	// Notified only once a host has failed REPEATEDLY. A single failed check is
	// an ordinary thing on the public internet, and a message for each one
	// would be noise that trains an operator to ignore the channel.
	//
	// The HOST only. Never the URL, never the error: a private registry's error
	// text has been known to carry the credential that failed.
	if failures == registryUnavailableAfterFailures {
		NotifyRegistryUnavailable(s.notifier, host, failures)
	}
}

// registryUnavailableAfterFailures is how many consecutive failures make a
// registry worth telling somebody about.
//
// Raised EXACTLY at the threshold rather than at or above it, so a registry
// that stays down produces one message rather than one per check until it
// recovers. The cooldown is a second line of defence, not the only one.
const registryUnavailableAfterFailures = 3

// hostSuccess clears a host's backoff.
func (s *ImageIntelService) hostSuccess(ctx context.Context, host string, kind domain.RegistryKind) {
	if host == "" {
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostWriteTimeout)
	defer cancel()

	if err := s.store.RecordHostOutcome(writeCtx, store.HostOutcome{
		Host:    host,
		Kind:    kind,
		Success: true,
	}, s.now().UTC()); err != nil {
		s.logger.Debug("could not record registry host success",
			slog.String("host", host), slog.String("error", err.Error()))
	}
}

// hostWriteTimeout bounds a health write, which must never outlive the check
// that produced it by much.
const hostWriteTimeout = 5 * time.Second

// persist writes the outcome and the history it produced.
func (s *ImageIntelService) persist(
	ctx context.Context,
	previous domain.ImageIntel,
	outcome store.CheckOutcome,
) error {
	outcome.Events = imageEvents(previous, outcome, s.now().UTC())

	// Detached from cancellation but bounded: a lookup that finished must not
	// lose its result to a shutdown mid-write, and must not hold the process
	// open either.
	writeCtx, cancel := GraceContext(ctx, imageWriteGrace, imageWriteMax)
	defer cancel()

	if err := s.store.RecordCheck(writeCtx, outcome, s.now().UTC()); err != nil {
		return err
	}

	if outcome.Status == domain.CheckOK {
		normalized, err := domain.NormalizeImageRef(outcome.Reference)
		if err == nil {
			s.hostSuccess(writeCtx, normalized.Host, normalized.Kind)
		}
	}
	return nil
}

// imageWriteGrace and imageWriteMax bound the persistence write at shutdown.
const (
	imageWriteGrace = 5 * time.Second
	imageWriteMax   = 30 * time.Second
)

// imageEvents derives the history rows one outcome produced.
//
// Only actual CHANGES become events. A pass that found everything unchanged
// writes nothing, which is what keeps the history readable and bounds its
// growth to the rate the world actually changes rather than the refresh
// interval.
func imageEvents(previous domain.ImageIntel, outcome store.CheckOutcome, now time.Time) []domain.ImageUpdateEvent {
	events := make([]domain.ImageUpdateEvent, 0, 2)

	event := func(kind domain.ImageEventKind, detail string) domain.ImageUpdateEvent {
		return domain.ImageUpdateEvent{
			Reference:  outcome.Reference,
			ObservedAt: now,
			Kind:       kind,
			Status:     outcome.Status,
			Detail:     detail,
		}
	}

	if outcome.Status != domain.CheckOK {
		// A failure is recorded once, on the transition. Recording it every
		// pass would fill the history with a repeating line.
		if previous.Status == domain.CheckOK || previous.Status == domain.CheckPending {
			events = append(events, event(domain.ImageEventCheckFailed, outcome.Detail))
		}
		return events
	}

	if previous.Status != domain.CheckOK && previous.Status != domain.CheckPending {
		events = append(events, event(domain.ImageEventCheckRecovered,
			"the registry is answering again"))
	}

	switch {
	case previous.RemoteDigest == "" && outcome.RemoteDigest != "":
		discovered := event(domain.ImageEventDiscovered, "the registry digest was resolved for the first time")
		discovered.CurrentDigest = outcome.RemoteDigest
		events = append(events, discovered)

	case outcome.RemoteDigest != "" && previous.RemoteDigest != outcome.RemoteDigest:
		changed := event(domain.ImageEventDigestChanged, "the registry now serves a different digest")
		changed.PreviousDigest = previous.RemoteDigest
		changed.CurrentDigest = outcome.RemoteDigest
		events = append(events, changed)
	}

	if previous.Update != outcome.Update {
		switch {
		case outcome.Update.Available():
			found := event(domain.ImageEventUpdateFound, outcome.UpdateReason)
			found.PreviousUpdate = previous.Update
			found.CurrentUpdate = outcome.Update
			found.LatestTag = outcome.LatestTag
			events = append(events, found)

		case previous.Update.Available():
			cleared := event(domain.ImageEventUpdateCleared,
				"the previously reported update is no longer available")
			cleared.PreviousUpdate = previous.Update
			cleared.CurrentUpdate = outcome.Update
			events = append(events, cleared)
		}
	}

	return events
}

// -------------------------------------------------------------- schedule --

// nextCheck returns when a successful reference should be looked at again.
//
// JITTERED. Without it every reference discovered in the same inventory refresh
// would come due in the same instant on every subsequent interval, turning a
// smooth trickle of requests into a synchronised burst at one registry -- which
// is exactly what earns a rate limit.
func (s *ImageIntelService) nextCheck(_ int) time.Time {
	interval := s.cfg.RefreshInterval
	// Up to 10% of the interval, added rather than subtracted so the configured
	// value is a floor.
	spread := interval / 10
	if spread > 0 {
		// Scheduling jitter, not a secret. An attacker who could predict when
		// HarborMaster next asks a public registry about a public image learns
		// nothing worth having, and crypto/rand here would buy nothing.
		//nolint:gosec // scheduling jitter; unpredictability is not a security property here.
		interval += time.Duration(rand.Int64N(int64(spread)))
	}
	return s.now().Add(interval)
}

// nextCheckAfterFailure returns when a failed reference or host should be
// retried, with exponential backoff.
//
// Capped, and jittered for the same reason as above: a registry that came back
// after an outage must not be met with every reference retrying at once.
func (s *ImageIntelService) nextCheckAfterFailure(failures int) time.Time {
	if failures < 1 {
		failures = 1
	}

	backoff := s.cfg.FailureBackoff
	// Doubling, with the shift bounded before it is applied so a long-failing
	// reference cannot overflow the duration.
	for attempt := 1; attempt < failures && backoff < s.cfg.MaxFailureBackoff; attempt++ {
		backoff *= 2
	}
	if backoff > s.cfg.MaxFailureBackoff {
		backoff = s.cfg.MaxFailureBackoff
	}

	spread := backoff / 4
	if spread > 0 {
		//nolint:gosec // scheduling jitter; see nextCheck.
		backoff += time.Duration(rand.Int64N(int64(spread)))
	}
	return s.now().Add(backoff)
}

// pickPlatform chooses the platform to record for an index, and reports whether
// the local platform was absent from it.
//
// The LOCAL platform is preferred when the index advertises it: a
// multi-architecture image resolves differently per platform, and the one that
// matters is the one the container is actually running.
//
// The second return value is the part that cannot be inferred later. An index
// that does not list the local platform yields NO platform rather than some
// other platform's -- a quietly wrong answer would be worse than none -- and
// that empty result is indistinguishable from "never determined". So the
// distinction is reported here, where it is still known.
func pickPlatform(local domain.Platform, advertised []domain.Platform) (domain.Platform, bool) {
	// No advertised list means a single-platform manifest rather than an index.
	// It establishes nothing about platform support, which is not the same as
	// establishing that support is absent.
	if len(advertised) == 0 {
		return domain.Platform{}, false
	}
	for _, candidate := range advertised {
		if candidate.OS == local.OS && candidate.Architecture == local.Architecture {
			return candidate, false
		}
	}
	if local.Empty() {
		return advertised[0], false
	}
	return domain.Platform{}, true
}

// parsePublished reads the OCI created annotation.
//
// Refuses anything that is not RFC3339 rather than guessing: a publisher's
// annotation is untrusted text, and a mis-parsed timestamp would render as a
// confident wrong date.
func parsePublished(annotations map[string]string) *time.Time {
	raw, ok := annotations["created"]
	if !ok || raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	// A timestamp far outside plausible range is a publisher error or a
	// deliberate oddity; either way it is not worth displaying.
	year := parsed.Year()
	if year < 2000 || year > 2200 {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}
