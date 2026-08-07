package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Update policy administration.
//
// # Why this is a service and not a handler
//
// An update policy is the most consequential object an administrator can
// create: it is the only one that can cause the host to change with nobody
// watching. So the rules about what a policy may say live here, once, on the
// path every caller takes -- rather than in a handler, where a second endpoint
// added later could reach the repository without them.
//
// Three things happen on every write, in this order:
//
//  1. NORMALISE. Trim, deduplicate, and fill defaults, so what is validated is
//     what will be stored and what will be stored is what the scheduler reads.
//  2. VALIDATE. Against the bounds and the closed vocabularies. A policy that
//     automates a verdict asking for human review is refused here, and refused
//     again by a CHECK constraint.
//  3. AUDIT. Who created the rule that later acted, with what mode and what
//     strategy. When automation changes a host at 02:14, this is the row that
//     says whose decision that was.

// UpdatePolicyStore is the persistence the service needs.
type UpdatePolicyStore interface {
	CreateUpdatePolicy(ctx context.Context, policy domain.UpdatePolicy, now time.Time) (domain.UpdatePolicy, error)
	ApplyUpdatePolicy(ctx context.Context, policyID string, change store.UpdatePolicyChange, now time.Time) (domain.UpdatePolicy, error)
	ArchiveUpdatePolicy(ctx context.Context, policyID string, now time.Time) error
	UpdatePolicyByID(ctx context.Context, policyID string) (domain.UpdatePolicy, error)
	ListUpdatePolicies(ctx context.Context, filter store.UpdatePolicyFilter) ([]domain.UpdatePolicy, int, error)
	ActivePolicies(ctx context.Context) ([]domain.UpdatePolicy, error)
	CountUpdatePolicies(ctx context.Context) (total, enabled int, err error)
}

// UpdatePolicyOptions configures an UpdatePolicyService.
type UpdatePolicyOptions struct {
	Store  UpdatePolicyStore
	Audit  *AuditRecorder
	Limits domain.UpdatePolicyLimits
	Logger *slog.Logger
	Now    func() time.Time
}

// UpdatePolicyService owns the automation rules.
//
// It holds no Docker capability and no reference to the engine. Creating a
// policy records an intention; whether that intention is ever acted on is the
// scheduler's business, and it re-reads the policies from the database on every
// pass.
type UpdatePolicyService struct {
	store  UpdatePolicyStore
	audit  *AuditRecorder
	limits domain.UpdatePolicyLimits
	logger *slog.Logger
	now    func() time.Time
}

// NewUpdatePolicyService builds an UpdatePolicyService.
func NewUpdatePolicyService(opts UpdatePolicyOptions) *UpdatePolicyService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &UpdatePolicyService{
		store:  opts.Store,
		audit:  opts.Audit,
		limits: opts.Limits,
		logger: logger,
		now:    now,
	}
}

// UpdatePolicyResult is a stored policy plus the warnings it earned.
//
// Warnings are returned rather than refused: an administrator may genuinely
// want an unattended major-version update at any hour, and they should just
// have to see HarborMaster say so first.
type UpdatePolicyResult struct {
	Policy   domain.UpdatePolicy `json:"policy"`
	Warnings []string            `json:"warnings,omitempty"`
}

// Create stores a new rule.
//
// The policy id is generated HERE, from the system entropy source, and the
// caller's is discarded if they sent one. An identifier a caller could choose
// is an identifier a caller could collide with, and a decision row references
// this one.
func (s *UpdatePolicyService) Create(
	ctx context.Context,
	policy domain.UpdatePolicy,
	actor Actor,
) (UpdatePolicyResult, error) {
	policy.PolicyID = domain.NewUpdatePolicyID()
	// Archived is not something a create may set. A policy that arrived
	// pre-withdrawn would be a row nobody can edit and nothing evaluates.
	policy.Archived = false

	policy.Normalise()
	if err := policy.Validate(s.limits); err != nil {
		return UpdatePolicyResult{}, err
	}

	created, err := s.store.CreateUpdatePolicy(ctx, policy, s.now())
	if err != nil {
		return UpdatePolicyResult{}, err
	}

	s.recordAudit(ctx, domain.AuditUpdatePolicyCreated, domain.AuditSucceeded,
		created, actor, "created an update policy in "+string(created.Mode)+
			" mode with a "+string(created.Strategy)+" ceiling")

	return UpdatePolicyResult{Policy: created, Warnings: created.Warnings()}, nil
}

// Update applies a change to an existing rule.
//
// Read-modify-validate-write: the change is merged onto the stored policy and
// the WHOLE result is validated, not just the fields that moved. A change that
// is individually legal can produce a policy that is not -- lowering
// maxConcurrent below maxPerRegistry, for instance -- and validating only the
// delta would store it.
func (s *UpdatePolicyService) Update(
	ctx context.Context,
	policyID string,
	change store.UpdatePolicyChange,
	actor Actor,
) (UpdatePolicyResult, error) {
	if !domain.ValidUpdatePolicyID(policyID) {
		return UpdatePolicyResult{}, store.ErrNotFound
	}

	existing, err := s.store.UpdatePolicyByID(ctx, policyID)
	if err != nil {
		return UpdatePolicyResult{}, err
	}

	preview := applyUpdatePolicyChange(existing, change)
	preview.Normalise()
	if err := preview.Validate(s.limits); err != nil {
		return UpdatePolicyResult{}, err
	}
	// The normalised values are what gets written, so the repository stores the
	// same policy that was validated rather than the caller's raw one.
	change = normalisedChange(change, preview)

	updated, err := s.store.ApplyUpdatePolicy(ctx, policyID, change, s.now())
	if err != nil {
		return UpdatePolicyResult{}, err
	}

	s.recordAudit(ctx, domain.AuditUpdatePolicyUpdated, domain.AuditSucceeded,
		updated, actor, describePolicyChange(existing, updated))

	return UpdatePolicyResult{Policy: updated, Warnings: updated.Warnings()}, nil
}

// Archive withdraws a rule.
//
// There is no delete. Automation decisions and pauses reference this row, and
// an auditor asking what changed the estate in March must not get a different
// answer because somebody tidied up in April.
func (s *UpdatePolicyService) Archive(
	ctx context.Context,
	policyID string,
	actor Actor,
) error {
	if !domain.ValidUpdatePolicyID(policyID) {
		return store.ErrNotFound
	}
	existing, err := s.store.UpdatePolicyByID(ctx, policyID)
	if err != nil {
		return err
	}
	if err := s.store.ArchiveUpdatePolicy(ctx, policyID, s.now()); err != nil {
		return err
	}
	s.recordAudit(ctx, domain.AuditUpdatePolicyArchived, domain.AuditSucceeded,
		existing, actor, "withdrew an update policy from evaluation")
	return nil
}

// Get reads one rule.
func (s *UpdatePolicyService) Get(ctx context.Context, policyID string) (UpdatePolicyResult, error) {
	if !domain.ValidUpdatePolicyID(policyID) {
		return UpdatePolicyResult{}, store.ErrNotFound
	}
	policy, err := s.store.UpdatePolicyByID(ctx, policyID)
	if err != nil {
		return UpdatePolicyResult{}, err
	}
	return UpdatePolicyResult{Policy: policy, Warnings: policy.Warnings()}, nil
}

// List returns a bounded page.
func (s *UpdatePolicyService) List(
	ctx context.Context,
	filter store.UpdatePolicyFilter,
) ([]domain.UpdatePolicy, int, error) {
	return s.store.ListUpdatePolicies(ctx, filter)
}

// Preview reports which of the estate's containers a candidate policy would
// govern, WITHOUT storing it.
//
// The question an administrator actually has before saving a selector is "what
// does this match", and answering it after the fact -- by saving the rule and
// reading the next pass -- means the rule was live in between. The targets come
// from the inventory; the policy is the caller's candidate and is validated
// first, so an unbounded selector cannot be evaluated even in a preview.
func (s *UpdatePolicyService) Preview(
	ctx context.Context,
	policy domain.UpdatePolicy,
	targets []store.AutomationTarget,
) ([]domain.SelectionTarget, error) {
	policy.Normalise()
	// A preview does not need a name or an id, but it must not be a way to run
	// an unbounded selector: validate the selector and the window, which are
	// the only parts a preview uses.
	if policy.Name == "" {
		policy.Name = "preview"
	}
	if policy.PolicyID == "" {
		policy.PolicyID = domain.NewUpdatePolicyID()
	}
	if err := policy.Validate(s.limits); err != nil {
		return nil, err
	}

	// Enabled for the purposes of matching only. A disabled policy governs
	// nothing, and previewing one would always return an empty list, which
	// looks like a broken selector rather than a disabled rule.
	policy.Enabled = true
	policy.Archived = false

	matched := make([]domain.SelectionTarget, 0, 16)
	for _, target := range targets {
		if policy.Governs(target.Selection) {
			matched = append(matched, target.Selection)
		}
	}
	return matched, nil
}

// ------------------------------------------------------------ internals --

// applyUpdatePolicyChange merges a change onto a stored policy.
func applyUpdatePolicyChange(policy domain.UpdatePolicy, change store.UpdatePolicyChange) domain.UpdatePolicy {
	if change.Name != nil {
		policy.Name = *change.Name
	}
	if change.Description != nil {
		policy.Description = *change.Description
	}
	if change.Enabled != nil {
		policy.Enabled = *change.Enabled
	}
	if change.Priority != nil {
		policy.Priority = *change.Priority
	}
	if change.Selector != nil {
		policy.Selector = *change.Selector
	}
	if change.Strategy != nil {
		policy.Strategy = *change.Strategy
	}
	if change.MinimumRecommendation != nil {
		policy.MinimumRecommendation = *change.MinimumRecommendation
	}
	if change.Mode != nil {
		policy.Mode = *change.Mode
	}
	if change.Window != nil {
		policy.Window = *change.Window
	}
	if change.Limits != nil {
		policy.Limits = *change.Limits
	}
	if change.Failure != nil {
		policy.Failure = *change.Failure
	}
	return policy
}

// normalisedChange rewrites a change to carry the normalised values.
//
// Only the fields the caller actually supplied are rewritten, so an omitted
// field stays omitted and the repository's "leave it alone" behaviour holds.
// The LIMITS and FAILURE fields are the exception: normalisation fills their
// zero values with defaults, and a caller who supplied a partial structure must
// get the defaulted one rather than the zeros.
func normalisedChange(change store.UpdatePolicyChange, normalised domain.UpdatePolicy) store.UpdatePolicyChange {
	if change.Name != nil {
		name := normalised.Name
		change.Name = &name
	}
	if change.Description != nil {
		description := normalised.Description
		change.Description = &description
	}
	if change.Selector != nil {
		selector := normalised.Selector
		change.Selector = &selector
	}
	if change.Strategy != nil {
		strategy := normalised.Strategy
		change.Strategy = &strategy
	}
	if change.MinimumRecommendation != nil {
		recommendation := normalised.MinimumRecommendation
		change.MinimumRecommendation = &recommendation
	}
	if change.Mode != nil {
		mode := normalised.Mode
		change.Mode = &mode
	}
	if change.Window != nil {
		window := normalised.Window
		change.Window = &window
	}
	if change.Limits != nil {
		limits := normalised.Limits
		change.Limits = &limits
	}
	if change.Failure != nil {
		failure := normalised.Failure
		change.Failure = &failure
	}
	return change
}

// describePolicyChange renders what an edit did, in HarborMaster's own words.
//
// Names the fields that moved and the values they moved BETWEEN for the two
// that decide how dangerous the policy is -- the mode and the strategy. Never
// echoes free text: a description an administrator typed does not belong in the
// audit log's reason column, where it would be rendered in a security page.
func describePolicyChange(before, after domain.UpdatePolicy) string {
	changes := make([]string, 0, 4)
	if before.Mode != after.Mode {
		changes = append(changes, "mode "+string(before.Mode)+" to "+string(after.Mode))
	}
	if before.Strategy != after.Strategy {
		changes = append(changes, "strategy "+string(before.Strategy)+" to "+string(after.Strategy))
	}
	if before.Enabled != after.Enabled {
		if after.Enabled {
			changes = append(changes, "enabled")
		} else {
			changes = append(changes, "disabled")
		}
	}
	if before.Failure.AutoRollback != after.Failure.AutoRollback {
		if after.Failure.AutoRollback {
			changes = append(changes, "automatic rollback on")
		} else {
			changes = append(changes, "automatic rollback off")
		}
	}
	if len(changes) == 0 {
		return "edited an update policy"
	}
	summary := "edited an update policy: "
	for index, change := range changes {
		if index > 0 {
			summary += ", "
		}
		summary += change
	}
	return summary
}

// recordAudit appends one policy administration event.
func (s *UpdatePolicyService) recordAudit(
	ctx context.Context,
	action domain.AuditAction,
	outcome domain.AuditOutcome,
	policy domain.UpdatePolicy,
	actor Actor,
	reason string,
) {
	if s.audit == nil {
		return
	}
	writeCtx, cancel := GraceContext(ctx, automationAuditGrace, automationAuditGrace)
	defer cancel()

	s.audit.RecordAction(writeCtx, actor, action, outcome, domain.AuditTargetUpdatePolicy,
		policy.PolicyID,
		// The policy NAME is administrator-typed text and is bounded and
		// sanitised before it reaches a column an operator reads.
		domain.SanitiseDisplayText(policy.Name, domain.MaxAuditTargetIDBytes),
		reason)
}

// ErrUpdatePolicyNotFound is returned when a policy id names nothing.
var ErrUpdatePolicyNotFound = errors.New("no update policy with that identifier")
