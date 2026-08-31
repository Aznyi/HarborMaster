package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// One container's chosen update behaviour (C2).
//
// # What this service does, and what it refuses to do
//
// It stores a preference and reports what that preference actually amounts to.
// It performs no container operation: there is no Docker capability on this
// type, so "changing a setting recreated my container" is not a bug this can
// have.
//
// The EFFECTIVE behaviour is not computed here. It is read from the automation
// engine's own decision for the container, produced by the same evaluation the
// scheduler runs. That is deliberate and it is the whole reason the interface
// can be truthful: a second implementation of "what would happen to this
// container" would eventually disagree with the first, and the disagreement
// would be invisible until somebody's database restarted at 3am.

// ContainerPreferenceStore is the persistence this service needs.
type ContainerPreferenceStore interface {
	SetContainerPreference(ctx context.Context, preference domain.ContainerUpdatePreference,
		actorID, actorName string, now time.Time) (domain.ContainerUpdatePreference, error)
	ClearContainerPreference(ctx context.Context, containerName string) error
	ContainerPreference(ctx context.Context, containerName string) (domain.ContainerUpdatePreference, error)
	ListContainerPreferences(ctx context.Context) ([]domain.ContainerUpdatePreference, error)
	// Resolved against the inventory in ONE query, so the summary can tell an
	// active preference from a saved one whose container is gone without asking
	// per row.
	ListContainerPreferencesWithPresence(ctx context.Context) ([]store.ContainerPreferenceRow, error)
}

// ContainerLookup resolves a container id to the name preferences are keyed by.
//
// Narrow on purpose. The preference service needs one fact about a container --
// its name -- and handing it the inventory repository would let it read
// configuration it has no business seeing.
type ContainerLookup interface {
	ContainerNameByID(ctx context.Context, containerID string) (string, error)
}

// AutomationDecisions supplies the engine's own verdict for a container.
//
// Satisfied by *AutomationService. An interface rather than the concrete type
// so this service cannot reach the engine's commands -- it may ask what would
// happen, and it may not cause anything to.
type AutomationDecisions interface {
	Upcoming(ctx context.Context) ([]domain.AutomationDecision, error)
}

// ContainerPreferenceOptions configures the service.
type ContainerPreferenceOptions struct {
	Store      ContainerPreferenceStore
	Containers ContainerLookup
	// Automation is optional. Without it the requested behaviour is still
	// readable and settable; the effective behaviour is reported as unknown
	// rather than guessed.
	Automation AutomationDecisions
	Audit      *AuditRecorder
	Logger     *slog.Logger
	Now        func() time.Time
}

// ContainerPreferenceService owns per-container update behaviour.
type ContainerPreferenceService struct {
	store      ContainerPreferenceStore
	containers ContainerLookup
	automation AutomationDecisions
	audit      *AuditRecorder
	logger     *slog.Logger
	now        func() time.Time
}

// NewContainerPreferenceService builds a ContainerPreferenceService.
func NewContainerPreferenceService(opts ContainerPreferenceOptions) *ContainerPreferenceService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &ContainerPreferenceService{
		store:      opts.Store,
		containers: opts.Containers,
		automation: opts.Automation,
		audit:      opts.Audit,
		logger:     logger,
		now:        now,
	}
}

// ErrUnknownBehavior reports a behaviour outside the closed vocabulary.
var ErrUnknownBehavior = errors.New(
	"update behavior must be one of automatic, reviewFirst, monitorOnly")

// ErrContainerUnknown reports a container the inventory does not hold.
var ErrContainerUnknown = errors.New("container not found")

// ContainerUpdateBehavior is what an operator asked for and what they get.
//
// The two are separate fields because they genuinely differ: a preference may
// only narrow the governing policy, so a container an update policy holds for
// review stays held however the dropdown is set. An interface that showed only
// the saved value would tell an operator their database updates automatically
// when it does not.
type ContainerUpdateBehavior struct {
	ContainerName string `json:"containerName"`

	// Requested is the stored preference, absent when nobody has chosen.
	Requested *domain.ContainerUpdatePreference `json:"requested,omitempty"`

	// Effective is what the engine would actually do, taken from its own
	// decision rather than re-derived here.
	Effective ContainerEffectiveBehavior `json:"effective"`
}

// ContainerEffectiveBehavior is the engine's verdict, in the words it used.
type ContainerEffectiveBehavior struct {
	// Known is false when the engine could not be consulted -- automation is
	// not configured, or the container was not in the evaluated estate. The
	// interface must say so rather than assume a behaviour.
	Known bool `json:"known"`

	// Verdict and Reason are the engine's own vocabulary, unmodified.
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// PolicyID and PolicyName attribute the decision, so the interface can say
	// WHICH rule overrode a preference instead of merely that one did.
	PolicyID   string `json:"policyId,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
}

// Behavior reports what one container asked for and what it gets.
func (s *ContainerPreferenceService) Behavior(
	ctx context.Context,
	containerID string,
) (ContainerUpdateBehavior, error) {
	name, err := s.containerName(ctx, containerID)
	if err != nil {
		return ContainerUpdateBehavior{}, err
	}

	result := ContainerUpdateBehavior{ContainerName: name}

	stored, err := s.store.ContainerPreference(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Nobody has chosen. Not an error: the container inherits.
	case err != nil:
		return ContainerUpdateBehavior{}, err
	default:
		result.Requested = &stored
	}

	result.Effective = s.effectiveFor(ctx, name)
	return result, nil
}

// SetBehavior records an operator's choice.
//
// Writes one row. Touches no container, reaches no Docker capability, and
// creates no acquisition, execution or rollback record.
func (s *ContainerPreferenceService) SetBehavior(
	ctx context.Context,
	containerID string,
	behavior domain.UpdateBehavior,
	actor Actor,
) (ContainerUpdateBehavior, error) {
	if !domain.ValidUpdateBehavior(string(behavior)) {
		return ContainerUpdateBehavior{}, ErrUnknownBehavior
	}
	name, err := s.containerName(ctx, containerID)
	if err != nil {
		return ContainerUpdateBehavior{}, err
	}

	stored, err := s.store.SetContainerPreference(ctx, domain.ContainerUpdatePreference{
		ContainerName: name,
		Behavior:      behavior,
		// Evidence of what was observed, never the identity.
		ContainerID: containerID,
	}, actor.UserID, actor.Username, s.now().UTC())
	if err != nil {
		return ContainerUpdateBehavior{}, err
	}

	s.recordAudit(ctx, domain.AuditContainerBehaviorSet, name, actor,
		"set this container to be "+behavior.Describe())

	return ContainerUpdateBehavior{
		ContainerName: name,
		Requested:     &stored,
		Effective:     s.effectiveFor(ctx, name),
	}, nil
}

// ClearBehavior removes a container's choice, returning it to what the
// governing policy says.
func (s *ContainerPreferenceService) ClearBehavior(
	ctx context.Context,
	containerID string,
	actor Actor,
) (ContainerUpdateBehavior, error) {
	name, err := s.containerName(ctx, containerID)
	if err != nil {
		return ContainerUpdateBehavior{}, err
	}
	if err := s.store.ClearContainerPreference(ctx, name); err != nil {
		return ContainerUpdateBehavior{}, err
	}

	s.recordAudit(ctx, domain.AuditContainerBehaviorCleared, name, actor,
		"cleared this container's update behaviour; it follows the governing policy again")

	return ContainerUpdateBehavior{
		ContainerName: name,
		Effective:     s.effectiveFor(ctx, name),
	}, nil
}

// List reports every stored preference, for the automation summary.
func (s *ContainerPreferenceService) List(
	ctx context.Context,
) ([]domain.ContainerUpdatePreference, error) {
	return s.store.ListContainerPreferences(ctx)
}

// ContainerBehaviorSummary is what the Automation workspace shows.
//
// # What it counts, and what it deliberately does not
//
// It counts STORED CHOICES -- the three behaviours C2 persists -- for
// containers that are currently on the host. It does not count effective
// decisions: `labelDisabled`, `selfUpdate`, `paused` and `observeMode` are
// things the ENGINE concluded, not things an operator saved here, and adding
// them to this total would make "3 containers have custom behaviour" mean
// something nobody chose.
//
// It also performs no effective evaluation. Reading the engine's verdict per
// container is an estate evaluation each, which is exactly the N+1 an overview
// page must not introduce.
type ContainerBehaviorSummary struct {
	// Items is every stored choice, present containers and absent ones alike,
	// each saying which it is. Bounded by the repository.
	Items []ContainerBehaviorItem `json:"items"`

	// Counts covers PRESENT containers only, by requested behaviour. Every
	// behaviour in the vocabulary has a key, so a client renders a real zero
	// rather than a missing one.
	Counts map[domain.UpdateBehavior]int `json:"counts"`

	// Total is how many present containers carry a stored choice: the sum of
	// Counts, stated once so a client need not add up to find the headline.
	Total int `json:"total"`

	// Stale is how many saved choices name a container that is not here.
	//
	// Reported rather than hidden or deleted. A preference is keyed by name so
	// it survives the recreation it authorises, which means one can outlive its
	// container; that row is inert, and counting it among active containers
	// would overstate what is configured.
	Stale int `json:"stale"`
}

// ContainerBehaviorItem is one stored choice.
type ContainerBehaviorItem struct {
	ContainerName string                `json:"containerName"`
	Behavior      domain.UpdateBehavior `json:"behavior"`
	// Present is false for a saved choice whose container is not on the host.
	Present bool `json:"present"`
	// ContainerID is the container's id NOW, and is absent when it is not here.
	// Never the id stored with the preference: that is evidence of what was
	// observed when the choice was made, and a recreation has changed it.
	ContainerID string `json:"containerId,omitempty"`
}

// BehaviorSummary reports which containers deviate from what they inherit.
//
// ONE query behind it, and no mutation of any kind.
func (s *ContainerPreferenceService) BehaviorSummary(
	ctx context.Context,
) (ContainerBehaviorSummary, error) {
	rows, err := s.store.ListContainerPreferencesWithPresence(ctx)
	if err != nil {
		return ContainerBehaviorSummary{}, err
	}

	// Every behaviour gets a key, so an absent count is a real zero on screen
	// rather than a gap a client has to guess the meaning of.
	counts := make(map[domain.UpdateBehavior]int, len(domain.UpdateBehaviors))
	for _, behavior := range domain.UpdateBehaviors {
		counts[behavior] = 0
	}

	summary := ContainerBehaviorSummary{
		Items:  make([]ContainerBehaviorItem, 0, len(rows)),
		Counts: counts,
	}
	for _, row := range rows {
		// A behaviour outside the vocabulary is not counted and not shown. The
		// column has a CHECK constraint so this cannot arise from HarborMaster,
		// and a value that reached the table another way must not become a
		// category on an operator's screen.
		if !domain.ValidUpdateBehavior(string(row.Behavior)) {
			continue
		}

		summary.Items = append(summary.Items, ContainerBehaviorItem{
			ContainerName: row.ContainerName,
			Behavior:      row.Behavior,
			Present:       row.Present,
			ContainerID:   row.CurrentContainerID,
		})
		if row.Present {
			counts[row.Behavior]++
			summary.Total++
		} else {
			summary.Stale++
		}
	}
	return summary, nil
}

// effectiveFor reads the engine's own decision for one container.
//
// Never re-derives it. When the engine cannot be consulted the result is
// reported as UNKNOWN rather than assumed, because "we could not ask" and "it
// will not be updated" are different answers and only one of them is honest.
func (s *ContainerPreferenceService) effectiveFor(
	ctx context.Context,
	name string,
) ContainerEffectiveBehavior {
	if s.automation == nil {
		return ContainerEffectiveBehavior{}
	}
	decisions, err := s.automation.Upcoming(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "the automation engine could not be consulted for a "+
			"container's effective update behaviour; reporting it as unknown",
			slog.String("containerName", name), slog.String("error", err.Error()))
		return ContainerEffectiveBehavior{}
	}
	for _, decision := range decisions {
		if decision.ContainerName != name {
			continue
		}
		return ContainerEffectiveBehavior{
			Known:      true,
			Verdict:    string(decision.Verdict),
			Reason:     string(decision.Reason),
			Detail:     decision.Detail,
			PolicyID:   decision.PolicyID,
			PolicyName: decision.PolicyName,
		}
	}
	return ContainerEffectiveBehavior{}
}

// containerName resolves the id a caller supplied to the NAME a preference is
// keyed by.
//
// Two reasons this is a lookup rather than trusting the caller. The name is the
// stable identity and the id is not, so the translation has to happen
// somewhere. And resolving through the inventory means a caller cannot store a
// preference for a container that does not exist, which is what stops the
// endpoint being a way to write arbitrary rows.
func (s *ContainerPreferenceService) containerName(
	ctx context.Context,
	containerID string,
) (string, error) {
	trimmed := strings.TrimSpace(containerID)
	if trimmed == "" {
		return "", ErrContainerUnknown
	}
	name, err := s.containers.ContainerNameByID(ctx, trimmed)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrContainerUnknown
	}
	if err != nil {
		return "", fmt.Errorf("resolve container name: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		return "", ErrContainerUnknown
	}
	return name, nil
}

// recordAudit records who changed a container's update behaviour.
func (s *ContainerPreferenceService) recordAudit(
	ctx context.Context,
	action domain.AuditAction,
	containerName string,
	actor Actor,
	reason string,
) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAction(ctx, actor, action, domain.AuditSucceeded,
		domain.AuditTargetContainer, containerName, containerName, reason)
}
