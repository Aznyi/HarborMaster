package service

import (
	"context"
	"errors"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The automatic-updates switch.
//
// # One switch, one ordinary policy, no second engine
//
// Everything below writes and reads exactly one UpdatePolicy -- the one at the
// reserved id -- through the SAME store, the SAME Normalise and the SAME
// Validate that an operator's own policy goes through. There is no branch
// anywhere in the scheduler, the planner, the acquisition service or the
// execution service that knows this policy is different, because it is not.
//
// # Off means DISABLED, not archived
//
// Archiving is how a policy becomes history: the repository refuses to edit an
// archived row, on the grounds that a decision references it. That is right for
// a policy an operator withdrew, and wrong for a switch, which has to be able
// to come back on. So "off" sets `enabled = false` and leaves the row in place.
//
// A disabled policy governs nothing -- `Governs` refuses it, and ActivePolicies
// does not even load it -- so off is off as far as the engine is concerned. It
// also means turning the switch off DELETES NOTHING: not the row, not the
// decisions that reference it, not a single user policy.

// SimpleUpdatesState is what the switch looks like right now.
type SimpleUpdatesState struct {
	// Enabled is true when the managed policy exists and is in force.
	Enabled bool `json:"enabled"`
	// Configured is true when the managed policy exists at all, in force or
	// not. It distinguishes "never turned on" from "turned off", which are
	// different sentences on screen.
	Configured bool `json:"configured"`

	// Policy is the managed rule, when there is one. Returned so the workspace
	// can describe the EFFECTIVE behaviour from the stored values rather than
	// restating them from a constant that might drift.
	Policy *domain.UpdatePolicy `json:"policy,omitempty"`
	// Warnings are the managed policy's own warnings, which are the honest
	// disclosure text for the confirmation dialog.
	Warnings []string `json:"warnings,omitempty"`

	// OverriddenBy names the user policies that outrank the managed one for at
	// least some containers. Reported rather than resolved: a narrower rule
	// winning is the designed behaviour, and an operator should be able to see
	// that it is happening rather than wonder why the switch seems inert.
	OverriddenBy []SimpleUpdatesOverride `json:"overriddenBy,omitempty"`
}

// SimpleUpdatesOverride is one user policy that takes precedence.
type SimpleUpdatesOverride struct {
	PolicyID string `json:"policyId"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	Priority int    `json:"priority"`
	Mode     string `json:"mode"`
}

// SimpleUpdates reports the switch's state.
func (s *UpdatePolicyService) SimpleUpdates(ctx context.Context) (SimpleUpdatesState, error) {
	managed, err := s.store.UpdatePolicyByID(ctx, domain.SimpleUpdatesPolicyID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Never turned on. Still report what would override it, so a fresh
		// install with existing policies can be told before it commits.
		overrides, err := s.simpleUpdatesOverrides(ctx)
		if err != nil {
			return SimpleUpdatesState{}, err
		}
		return SimpleUpdatesState{OverriddenBy: overrides}, nil
	case err != nil:
		return SimpleUpdatesState{}, err
	}

	overrides, err := s.simpleUpdatesOverrides(ctx)
	if err != nil {
		return SimpleUpdatesState{}, err
	}
	return SimpleUpdatesState{
		Enabled:      managed.Enabled && !managed.Archived,
		Configured:   true,
		Policy:       &managed,
		Warnings:     managed.Warnings(),
		OverriddenBy: overrides,
	}, nil
}

// EnableSimpleUpdates turns the switch on.
//
// Creates the managed policy the first time and re-enables it afterwards. The
// compiled fields are rewritten on every enable, so an installation that turns
// the switch on after an upgrade gets the CURRENT recommended behaviour rather
// than whatever the semantics were the first time it was used.
//
// Nothing is done to a container here. This writes one row; the next scheduler
// pass reads it, and every gate between that pass and a running container is
// unchanged.
func (s *UpdatePolicyService) EnableSimpleUpdates(
	ctx context.Context,
	actor Actor,
) (UpdatePolicyResult, error) {
	want := domain.SimpleUpdatesPolicy()
	want.Normalise()
	if err := want.Validate(s.limits); err != nil {
		// The compiled policy failing validation is a programming error, not a
		// caller error, and it must not be reported as though the operator did
		// something wrong.
		return UpdatePolicyResult{}, err
	}

	existing, err := s.store.UpdatePolicyByID(ctx, domain.SimpleUpdatesPolicyID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		created, err := s.store.CreateUpdatePolicy(ctx, want, s.now().UTC())
		if err != nil {
			return UpdatePolicyResult{}, err
		}
		s.recordAudit(ctx, domain.AuditUpdatePolicyCreated, domain.AuditSucceeded,
			created, actor, "turned on automatic updates, which covers "+
				created.Scope.Describe()+" with a "+string(created.Strategy)+" ceiling")
		return UpdatePolicyResult{Policy: created, Warnings: created.Warnings()}, nil

	case err != nil:
		return UpdatePolicyResult{}, err

	case existing.Archived:
		// Should not be reachable: the switch never archives. If somebody
		// archived the row by hand through the advanced surface, say so plainly
		// rather than silently creating a second broad policy.
		return UpdatePolicyResult{}, ErrSimpleUpdatesArchived
	}

	enabled := true
	updated, err := s.store.ApplyUpdatePolicy(ctx, domain.SimpleUpdatesPolicyID, store.UpdatePolicyChange{
		Name:                  &want.Name,
		Description:           &want.Description,
		Enabled:               &enabled,
		Priority:              &want.Priority,
		Scope:                 &want.Scope,
		Selector:              &want.Selector,
		Strategy:              &want.Strategy,
		MinimumRecommendation: &want.MinimumRecommendation,
		Mode:                  &want.Mode,
		Window:                &want.Window,
		Failure:               &want.Failure,
	}, s.now().UTC())
	if err != nil {
		return UpdatePolicyResult{}, err
	}
	s.recordAudit(ctx, domain.AuditUpdatePolicyUpdated, domain.AuditSucceeded,
		updated, actor, "turned on automatic updates, which covers "+
			updated.Scope.Describe()+" with a "+string(updated.Strategy)+" ceiling")
	return UpdatePolicyResult{Policy: updated, Warnings: updated.Warnings()}, nil
}

// DisableSimpleUpdates turns the switch off.
//
// Disables the managed policy and nothing else. No user policy is read, edited,
// archived or deleted; no container is touched; no decision, pause, approval or
// history row is removed. A switch that was never on is not an error -- off is
// the state the caller asked for and the state they get.
func (s *UpdatePolicyService) DisableSimpleUpdates(
	ctx context.Context,
	actor Actor,
) (UpdatePolicyResult, error) {
	existing, err := s.store.UpdatePolicyByID(ctx, domain.SimpleUpdatesPolicyID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return UpdatePolicyResult{}, nil
	case err != nil:
		return UpdatePolicyResult{}, err
	case existing.Archived || !existing.Enabled:
		return UpdatePolicyResult{Policy: existing}, nil
	}

	disabled := false
	updated, err := s.store.ApplyUpdatePolicy(ctx, domain.SimpleUpdatesPolicyID, store.UpdatePolicyChange{
		Enabled: &disabled,
	}, s.now().UTC())
	if err != nil {
		return UpdatePolicyResult{}, err
	}
	s.recordAudit(ctx, domain.AuditUpdatePolicyUpdated, domain.AuditSucceeded,
		updated, actor, "turned off automatic updates; policies written by hand are unaffected")
	return UpdatePolicyResult{Policy: updated}, nil
}

// ErrSimpleUpdatesArchived reports a managed policy somebody withdrew by hand.
var ErrSimpleUpdatesArchived = errors.New(
	"the managed automatic-updates policy has been archived and cannot be reinstated")

// simpleUpdatesOverrides lists the ACTIVE user policies that outrank the
// managed one.
//
// "Outranks" is not re-derived here. domain.SimpleUpdatesPolicy is built to
// lose every key of the ordering rule -- minimum priority, broadest scope, the
// largest possible id -- so any active policy an operator wrote beats it for
// every container that policy governs. What this reports is therefore simply
// "which policies exist besides the managed one", which is the question an
// operator is actually asking.
func (s *UpdatePolicyService) simpleUpdatesOverrides(
	ctx context.Context,
) ([]SimpleUpdatesOverride, error) {
	active, err := s.store.ActivePolicies(ctx)
	if err != nil {
		return nil, err
	}
	var out []SimpleUpdatesOverride
	for _, policy := range active {
		if domain.IsSimpleUpdatesPolicy(policy.PolicyID) {
			continue
		}
		out = append(out, SimpleUpdatesOverride{
			PolicyID: policy.PolicyID,
			Name:     policy.Name,
			Scope:    string(policy.Scope),
			Priority: policy.Priority,
			Mode:     string(policy.Mode),
		})
	}
	return out, nil
}
