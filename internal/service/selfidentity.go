package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Working out which container HarborMaster is running in.
//
// # Why this is not a Docker call
//
// The Docker API has no "which container am I" question. A container can ask
// about a container it can name, and naming itself is exactly the problem. So
// the id comes from the process's own view of itself — `/proc`, the hostname —
// and is then RESOLVED against the inventory HarborMaster already holds.
//
// Nothing here adds a method to docker.Runtime, and nothing here reads the
// socket. The probes are file reads and an environment lookup; the resolution
// is a query against HarborMaster's own database.
//
// # Every probe is allowed to fail
//
// Detection runs once at startup and is refreshed after each inventory commit.
// A probe that fails contributes nothing and is logged at debug; the next one
// is tried. What is NOT allowed is a probe that returns a wrong answer
// confidently, so each one either produces a full 64-character id or produces
// nothing.

// SelfIdentifier establishes and caches HarborMaster's own identity.
//
// Safe for concurrent use. The identity is read on every automation pass and
// written rarely, so it is stored behind the same mutex rather than an atomic —
// the value is a struct and copying it under a lock is cheaper than the
// indirection an atomic.Value would need.
type SelfIdentifier struct {
	containers SelfContainerReader
	logger     *slog.Logger

	// configured is an operator-supplied container id, trusted above every
	// probe. Somebody who knows beats a guess.
	configured string

	mu       sync.RWMutex
	identity domain.SelfIdentity
}

// SelfContainerReader is the inventory read the resolver needs.
//
// Deliberately narrow, and deliberately READ-ONLY: establishing what
// HarborMaster is must not be able to change anything.
type SelfContainerReader interface {
	List(ctx context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error)
}

// SelfIdentifierOptions configures a SelfIdentifier.
type SelfIdentifierOptions struct {
	Containers SelfContainerReader
	// ConfiguredID is HARBORMASTER_SELF_CONTAINER_ID, when an operator set it.
	ConfiguredID string
	Logger       *slog.Logger
}

// NewSelfIdentifier builds a SelfIdentifier.
func NewSelfIdentifier(opts SelfIdentifierOptions) *SelfIdentifier {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SelfIdentifier{
		containers: opts.Containers,
		logger:     logger,
		configured: strings.TrimSpace(opts.ConfiguredID),
		identity: domain.SelfIdentity{
			Source: domain.SelfSourceNone,
			Detail: "HarborMaster has not yet determined which container it is running in",
		},
	}
}

// Identity returns what HarborMaster currently believes about itself.
func (s *SelfIdentifier) Identity() domain.SelfIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.identity
}

// InventoryRefreshed implements the inventory service's RefreshObserver.
//
// Re-resolves after every committed refresh. The container id does not change
// while the process runs, but the NAME and the IMAGE can — a `docker rename`,
// or HarborMaster's own container being recreated by an operator's compose
// file — and a stale name would exclude the wrong container.
//
// Non-blocking by contract: the inventory service must never wait on an
// observer.
func (s *SelfIdentifier) InventoryRefreshed(int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), selfResolveTimeout)
		defer cancel()
		s.Resolve(ctx)
	}()
}

// selfResolveTimeout bounds one resolution.
const selfResolveTimeout = 10 * time.Second

// Resolve establishes the identity and caches it.
//
// Called at startup and after every inventory commit. Never returns an error:
// a failure to identify is a RESULT, recorded in the identity's Source and
// Detail so the API can report it, not a condition that stops anything.
func (s *SelfIdentifier) Resolve(ctx context.Context) domain.SelfIdentity {
	identity := s.probe(ctx)

	s.mu.Lock()
	previous := s.identity
	s.identity = identity
	s.mu.Unlock()

	// Logged once when it changes rather than on every refresh. An operator
	// wants to see this at startup and when it moves; ninety-six identical
	// lines a day is how a useful line becomes invisible.
	if previous.ContainerID != identity.ContainerID || previous.Source != identity.Source {
		if identity.Known() {
			s.logger.InfoContext(ctx, "identified HarborMaster's own container",
				slog.String("containerId", domain.ShortenID(identity.ContainerID)),
				slog.String("containerName", identity.ContainerName),
				slog.String("source", string(identity.Source)))
		} else {
			// A warning, not an error. Automation still runs; the exclusion is
			// simply weaker, and the API says so.
			s.logger.WarnContext(ctx, "could not identify HarborMaster's own container; "+
				"self-update protection will rely on the image and label signals only",
				slog.String("detail", identity.Detail))
		}
	}
	return identity
}

// probe runs the detection chain, strongest signal first.
func (s *SelfIdentifier) probe(ctx context.Context) domain.SelfIdentity {
	// 1. An operator who told us. Validated by shape: a configured value that
	// is not a container id is a mistake worth refusing rather than a hint
	// worth guessing from.
	if s.configured != "" {
		if !validFullContainerID(s.configured) {
			return domain.SelfIdentity{
				Source: domain.SelfSourceNone,
				Detail: "the configured self container id is not a full 64-character id and was ignored",
			}
		}
		return s.enrich(ctx, s.configured, domain.SelfSourceConfigured,
			"an operator named this container explicitly")
	}

	// 2. The process's own control group or mount table.
	if id, ok := containerIDFromProc(); ok {
		return s.enrich(ctx, id, domain.SelfSourceRuntime,
			"read from this process's own control group")
	}

	// 3. The hostname. The daemon sets it to the container's short id unless an
	// operator overrode it, so a match is strong evidence and a miss means
	// nothing.
	if id, ok := s.matchHostname(ctx); ok {
		return s.enrich(ctx, id, domain.SelfSourceHostname,
			"the hostname matches this container's short id")
	}

	// 4. A container that says it is HarborMaster.
	if identity, ok := s.matchSelfLabel(ctx); ok {
		return identity
	}

	return domain.SelfIdentity{
		Source: domain.SelfSourceNone,
		Detail: "HarborMaster is not running in a container, or its container could not be identified",
	}
}

// enrich resolves a container id against the inventory.
//
// The id alone is enough to refuse an update, but the name and image make the
// refusal legible and give the image-based signal something to compare. A
// container id that resolves to nothing is still returned: HarborMaster knows
// what it is even when the inventory has not caught up.
func (s *SelfIdentifier) enrich(
	ctx context.Context,
	containerID string,
	source domain.SelfIdentitySource,
	detail string,
) domain.SelfIdentity {
	identity := domain.SelfIdentity{
		ContainerID: strings.ToLower(containerID),
		Source:      source,
		Detail:      detail,
	}
	if s.containers == nil {
		return identity
	}

	summaries, _, err := s.containers.List(ctx, store.ContainerFilter{
		Page: store.Page{Limit: maxSelfScan},
	})
	if err != nil {
		return identity
	}
	for _, summary := range summaries {
		if !strings.EqualFold(summary.ID, identity.ContainerID) {
			continue
		}
		identity.ContainerName = summary.Name
		identity.ImageRef = summary.Image.Raw
		identity.ImageID = summary.ImageID
		break
	}
	return identity
}

// maxSelfScan bounds the inventory read the resolver performs.
//
// The resolver walks the container list once per refresh looking for one row.
// The bound is the same order as the automation target bound: past it, a host
// is large enough that the operator should be configuring the id explicitly.
const maxSelfScan = 2000

// matchHostname looks for a container whose short id is this host's hostname.
func (s *SelfIdentifier) matchHostname(ctx context.Context) (string, bool) {
	if s.containers == nil {
		return "", false
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", false
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	// A Docker-generated hostname is exactly the 12-character short id. A
	// shorter or longer one is an operator's own hostname, and matching on a
	// prefix of it would be a guess.
	if len(hostname) != 12 || !isLowerHex(hostname) {
		return "", false
	}

	summaries, _, err := s.containers.List(ctx, store.ContainerFilter{
		Page: store.Page{Limit: maxSelfScan},
	})
	if err != nil {
		return "", false
	}
	for _, summary := range summaries {
		if strings.HasPrefix(strings.ToLower(summary.ID), hostname) {
			return summary.ID, true
		}
	}
	return "", false
}

// matchSelfLabel looks for a container carrying the self-identifying label.
//
// The deployments HarborMaster ships set it. Last in the chain because it is
// the only operator-supplied signal, and present because it is the one that
// keeps working when a deployment does something the probes did not anticipate.
func (s *SelfIdentifier) matchSelfLabel(ctx context.Context) (domain.SelfIdentity, bool) {
	if s.containers == nil {
		return domain.SelfIdentity{}, false
	}
	summaries, _, err := s.containers.List(ctx, store.ContainerFilter{
		LabelKey: domain.LabelSelfIdentity,
		Page:     store.Page{Limit: 2},
	})
	if err != nil || len(summaries) != 1 {
		// Zero is "no label set". More than one is ambiguous, and picking one
		// would be a guess — every other signal still applies.
		return domain.SelfIdentity{}, false
	}
	summary := summaries[0]
	return domain.SelfIdentity{
		ContainerID:   summary.ID,
		ContainerName: summary.Name,
		ImageRef:      summary.Image.Raw,
		ImageID:       summary.ImageID,
		Source:        domain.SelfSourceLabel,
		Detail:        "a container carries the " + domain.LabelSelfIdentity + " label",
	}, true
}

// ------------------------------------------------------------- the probes --

// cgroupContainerID matches a 64-hex container id anywhere in a control-group
// or mount line.
//
// Anchored to a word boundary on both sides so a longer hex run — a layer
// digest in an overlay path, for instance — does not produce a truncated
// match. Compiled once: this runs on a refresh observer.
var cgroupContainerID = regexp.MustCompile(`(?:^|[^0-9a-f])([0-9a-f]{64})(?:[^0-9a-f]|$)`)

// procFiles are the places a container id may appear, in the order they are
// tried.
//
// `/proc/self/cgroup` carries it on cgroup v1 and on cgroup v2 under Docker's
// default driver. `/proc/self/mountinfo` carries it in the overlay upperdir
// path when the cgroup namespace hides it, which is the common cgroup v2 case
// under systemd.
var procFiles = []string{"/proc/self/cgroup", "/proc/self/mountinfo"}

// containerIDFromProc reads the container id from the process's own view.
//
// Returns false on any doubt. A partial match, an unreadable file, or a line
// too long to be a control-group entry all mean "not established", and the
// caller falls through to the next probe.
func containerIDFromProc() (string, bool) {
	for _, path := range procFiles {
		if id, ok := containerIDFromFile(path); ok {
			return id, true
		}
	}
	return "", false
}

// containerIDFromFile scans one file for a container id.
func containerIDFromFile(path string) (string, bool) {
	// The path comes from `procFiles`, a package-level constant list. There is
	// no caller-supplied path here and no way to introduce one: the parameter
	// exists so the two probes share one bounded reader.
	file, err := os.Open(path) //nolint:gosec // a fixed path from procFiles

	if err != nil {
		// Not on Linux, not in a container, or no permission. All three mean
		// the same thing here.
		return "", false
	}
	defer func() { _ = file.Close() }()

	// Bounded: a hostile or broken /proc must not make this allocate. The real
	// files are a few kilobytes.
	reader := bufio.NewReader(io.LimitReader(file, maxProcBytes))
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 4096), maxProcLineBytes)

	for scanner.Scan() {
		line := scanner.Text()
		// A container id in a mount line only counts when the line is one of
		// the container's own overlay mounts. Without this, a bind mount of
		// somebody else's container directory would be read as our identity.
		if strings.HasSuffix(path, "mountinfo") && !plausibleContainerMount(line) {
			continue
		}
		if match := cgroupContainerID.FindStringSubmatch(line); match != nil {
			return match[1], true
		}
	}
	return "", false
}

const (
	// maxProcBytes bounds one probe's read.
	maxProcBytes = 256 << 10
	// maxProcLineBytes bounds one line.
	maxProcLineBytes = 16 << 10
)

// plausibleContainerMount reports whether a mountinfo line describes the
// container's own storage rather than something bind-mounted into it.
//
// An allowlist of the paths a runtime uses for a container's own layers. A
// bind mount of `/var/lib/docker` from the host would otherwise let the FIRST
// container id in the file — some other container's — be read as this one's.
func plausibleContainerMount(line string) bool {
	for _, marker := range []string{
		"/docker/containers/",
		"/docker/overlay2/",
		"/containerd/",
		"/kubepods/",
		"/docker/btrfs/subvolumes/",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// validFullContainerID reports whether a value is a full 64-hex container id.
func validFullContainerID(value string) bool {
	if len(value) != 64 {
		return false
	}
	return isLowerHex(strings.ToLower(value))
}

// isLowerHex reports whether every character is a lowercase hex digit.
//
// Named apart from the identically-shaped helper in secret_key.go: the two
// answer the same question for unrelated reasons, and a shared one would couple
// the self-identity probe to the credential path.
func isLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return len(value) > 0
}

// ErrSelfUpdateRefused reports that an operation named HarborMaster itself.
//
// Distinguishable from every other refusal because the remedy is different:
// nothing an operator changes about the plan, the policy, or the window will
// make this succeed. Updating HarborMaster is a `docker compose pull &&
// docker compose up -d` performed from outside it.
var ErrSelfUpdateRefused = errors.New(
	"HarborMaster cannot update the container it is running in")
