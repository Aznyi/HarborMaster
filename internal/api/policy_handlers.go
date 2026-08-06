package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The policy endpoints.
//
// # What they can and cannot do
//
// Six reads and five writes. Every write acts on HARBORMASTER'S OWN ROWS: a
// policy definition, a violation's status, or a request to re-run an
// evaluation. None of them reaches Docker, none of them changes a container,
// and adding one that did would have to get past the architecture test that
// keeps the Docker SDK inside internal/docker.
//
// This is the first phase to add a create/update/delete surface, so it is worth
// being explicit about what that surface is: an administrator's list of rules
// that HarborMaster CHECKS configuration against. A policy is never applied,
// enforced, or pushed to the daemon.
//
// # Why DELETE archives
//
// A violation references its policy, and the history has to survive the rule
// being withdrawn -- an auditor asking what the estate was failing last quarter
// must not get a different answer because someone tidied up this quarter. So
// DELETE archives: the policy stops being evaluated, its open violations
// resolve in the same transaction, and the record of what it caught remains.
// The schema enforces the same thing through ON DELETE RESTRICT.
//
// # Why the writes are guarded the way they are
//
// Every write goes through guardPolicyWrite and, where it has a body,
// decodeJSONBody: JSON media type required (which forces a CORS preflight that
// fails, because no CORS headers are served), strict decoding with unknown
// fields rejected, a body size cap, UTF-8 validation on the raw bytes, a
// per-process rate limit, and the Sec-Fetch/Origin checks.
//
// The rate limit is the POLICY bucket rather than the shared one, and is more
// permissive: a limit should be proportional to what the request costs, and a
// policy write is one small transaction on HarborMaster's own table rather than
// a Docker sweep. See guardPolicyWrite.
//
// HarborMaster is unauthenticated by design and expects to sit on loopback or
// behind a reverse proxy; these controls bound abuse, they do not authenticate
// anyone. See docs/security/threat-model.md.

// PolicyStore is the policy capability the API depends on.
//
// A narrow interface rather than *store.PolicyRepository, so the handlers stay
// testable without a database and so the surface the API can reach is visible
// in one place. Note what is ABSENT: there is no DeletePolicy, and no method
// that resolves a violation. Resolution is something the world does, not
// something a caller asserts.
type PolicyStore interface {
	ListPolicies(ctx context.Context, filter store.PolicyFilter) ([]domain.PolicyDefinition, int, error)
	GetPolicy(ctx context.Context, policyID string) (domain.PolicyDefinition, error)
	CreatePolicy(ctx context.Context, policy domain.PolicyDefinition, now time.Time) (domain.PolicyDefinition, error)
	UpdatePolicy(ctx context.Context, policyID string, update store.PolicyUpdate, now time.Time) (domain.PolicyDefinition, error)
	ArchivePolicy(ctx context.Context, policyID string, now time.Time) error

	ListViolations(ctx context.Context, filter store.PolicyViolationFilter) ([]domain.PolicyViolation, int, error)
	GetViolation(ctx context.Context, id int64) (domain.PolicyViolation, error)
	UpdateViolationStatus(ctx context.Context, id int64, status domain.PolicyViolationStatus,
		note string, now time.Time) (domain.PolicyViolation, error)

	PolicySummary(ctx context.Context) (domain.PolicySummary, error)
	PolicyEvaluation(ctx context.Context, containerID string) (domain.PolicyEvaluation, error)
}

// PolicyEvaluator is the engine capability the manual endpoint needs.
//
// Deliberately narrow: the API can ASK for a pass and read the engine's status.
// It cannot evaluate synchronously, which is what keeps an unauthenticated
// caller from holding a request open across a thousand-container sweep.
type PolicyEvaluator interface {
	RequestSweep()
	Status() domain.PolicyEngineStatus
}

// policyUnavailable writes the disabled response, and reports whether it did.
func (s *Server) policyUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.policies != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"the policy engine is not configured")
	return true
}

// ------------------------------------------------------------ definitions --

// handlePolicies lists policy definitions.
func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}

	query, err := parsePolicyQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	policies, total, err := s.policies.ListPolicies(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.PolicyDefinition]{
		Items:      policies,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handlePolicyRules serves the rule catalogue.
//
// The policy editor is built from this rather than from a hand-maintained list
// in the frontend: a second copy would eventually offer a rule the backend
// rejects, and the operator would find out only after filling in the form.
func (s *Server) handlePolicyRules(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, policyRuleCatalogueResponse{
		Rules:         domain.PolicyRuleCatalogue(),
		Severities:    domain.PolicySeverities,
		RestartPolicy: domain.RestartPolicyNames,
		Limits:        s.policyLimits(),
	})
}

// policyRuleCatalogueResponse is what the editor needs to render every rule and
// to validate locally against the same bounds the server enforces.
type policyRuleCatalogueResponse struct {
	Rules      []domain.PolicyRuleSpec `json:"rules"`
	Severities []domain.PolicySeverity `json:"severities"`
	// RestartPolicy is the closed vocabulary a restart-policy rule may name.
	RestartPolicy []string            `json:"restartPolicyNames"`
	Limits        domain.PolicyLimits `json:"limits"`
}

// handlePolicyDetail returns one policy definition.
func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	policyID, ok := s.policyID(w, r)
	if !ok {
		return
	}

	policy, err := s.policies.GetPolicy(r.Context(), policyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, policy)
}

// policyRequest is the POST and PATCH body.
//
// Every field is a POINTER so "not supplied" and "supplied as the zero value"
// stay distinguishable. Without that, a PATCH omitting `enabled` would silently
// disable the policy, and one omitting `rules` would silently empty it.
//
// There is deliberately no policyId field: the identifier is server-generated
// and immutable, and a body that cannot express the change is a stronger
// guarantee than a check that rejects it. Unknown fields are rejected by the
// decoder, so a caller that tries finds out rather than being ignored.
type policyRequest struct {
	Name        *string              `json:"name"`
	Description *string              `json:"description"`
	Severity    *string              `json:"severity"`
	Enabled     *bool                `json:"enabled"`
	Rules       *[]domain.PolicyRule `json:"rules"`
}

// handlePolicyCreate stores a new policy definition.
func (s *Server) handlePolicyCreate(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}

	var request policyRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &request); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// A create must supply everything: an omitted field on a create is a
	// caller mistake, not an instruction to keep an existing value.
	if request.Name == nil || request.Severity == nil || request.Rules == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"name, severity and rules are required")
		return
	}

	policy := domain.PolicyDefinition{
		// Generated here, never taken from the body.
		PolicyID: domain.NewPolicyID(),
		Name:     *request.Name,
		Severity: domain.PolicySeverity(*request.Severity),
		// Enabled defaults to true: a policy an administrator just wrote is one
		// they want applied. Explicitly disabling it stays possible.
		Enabled: true,
		Rules:   *request.Rules,
	}
	if request.Description != nil {
		policy.Description = *request.Description
	}
	if request.Enabled != nil {
		policy.Enabled = *request.Enabled
	}

	// Normalise trims and deduplicates so the bounds below apply to what will
	// actually be stored, then Validate checks the catalogue and the limits.
	policy.Normalise()
	if err := policy.Validate(s.policyLimits()); err != nil {
		s.writePolicyValidationError(w, r, err)
		return
	}

	created, err := s.policies.CreatePolicy(r.Context(), policy, s.now())
	if errors.Is(err, store.ErrPolicyNameTaken) {
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"a policy with that name already exists")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy create failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// The new policy applies from now on, so the estate is re-evaluated rather
	// than waiting for the next sweep to notice. Non-blocking: the engine
	// coalesces and the request does not wait on the pass.
	s.requestPolicySweep()

	w.Header().Set("Location", APIPrefix+"/policies/"+created.PolicyID)
	s.auditWrite(r, domain.AuditPolicyCreated, domain.AuditTargetPolicy,
		created.PolicyID, created.Name, "policy created")

	writeJSON(w, r, s.logger, http.StatusCreated, created)
}

// handlePolicyUpdate applies a partial update.
func (s *Server) handlePolicyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	policyID, ok := s.policyID(w, r)
	if !ok {
		return
	}

	var request policyRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &request); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	existing, err := s.policies.GetPolicy(r.Context(), policyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// The update is validated as the WHOLE policy it will produce, not as the
	// changed fields in isolation. A rules list that is legal on its own can
	// still be illegal against the name it is landing next to, and a partial
	// check would let the second case through.
	merged := existing
	update := store.PolicyUpdate{}
	if request.Name != nil {
		merged.Name = *request.Name
		update.Name = &merged.Name
	}
	if request.Description != nil {
		merged.Description = *request.Description
		update.Description = &merged.Description
	}
	if request.Severity != nil {
		merged.Severity = domain.PolicySeverity(*request.Severity)
		update.Severity = &merged.Severity
	}
	if request.Enabled != nil {
		merged.Enabled = *request.Enabled
		update.Enabled = &merged.Enabled
	}
	if request.Rules != nil {
		merged.Rules = *request.Rules
	}

	// Normalise trims and deduplicates IN PLACE, and every pointer set above
	// already refers to a field of `merged`, so the name and description the
	// repository receives are the normalised ones without being reassigned.
	// The rules are the exception: Normalise replaces the slice, so the pointer
	// has to be taken afterwards.
	merged.Normalise()
	if err := merged.Validate(s.policyLimits()); err != nil {
		s.writePolicyValidationError(w, r, err)
		return
	}
	if request.Rules != nil {
		update.Rules = &merged.Rules
	}

	updated, err := s.policies.UpdatePolicy(r.Context(), policyID, update, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy not found")
		return
	case errors.Is(err, store.ErrPolicyNameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"a policy with that name already exists")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "policy update failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.requestPolicySweep()
	s.auditWrite(r, domain.AuditPolicyUpdated, domain.AuditTargetPolicy,
		updated.PolicyID, updated.Name, "policy updated")

	writeJSON(w, r, s.logger, http.StatusOK, updated)
}

// handlePolicyDelete archives a policy.
//
// ARCHIVES, not deletes. See the file comment: violations reference the policy
// and the history must survive the rule being withdrawn. The policy stops being
// evaluated and its open violations resolve in the same transaction, which is
// the truthful record of a rule that no longer applies.
func (s *Server) handlePolicyDelete(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	policyID, ok := s.policyID(w, r)
	if !ok {
		return
	}

	err := s.policies.ArchivePolicy(r.Context(), policyID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy archive failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.auditWrite(r, domain.AuditPolicyArchived, domain.AuditTargetPolicy,
		policyID, "", "policy withdrawn; its history is retained")

	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------- violations --

// handlePolicyViolations lists violations.
func (s *Server) handlePolicyViolations(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}

	query, err := parseViolationQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	violations, total, err := s.policies.ListViolations(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy violation list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.PolicyViolation]{
		Items:      violations,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handlePolicyViolationDetail returns one violation.
func (s *Server) handlePolicyViolationDetail(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	id, ok := s.violationID(w, r)
	if !ok {
		return
	}

	violation, err := s.policies.GetViolation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy violation not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy violation load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, violation)
}

// violationStatusRequest is the violation PATCH body.
type violationStatusRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// handlePolicyViolationPatch applies an operator's status transition.
//
// The transition does NOT suppress re-evaluation. The next pass still applies
// the rule, still refreshes the violation's last-seen time, and still resolves
// it the moment the container complies. An acknowledgement that stopped the
// checking would turn the compliance report into a list of things somebody once
// clicked.
func (s *Server) handlePolicyViolationPatch(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}
	id, ok := s.violationID(w, r)
	if !ok {
		return
	}

	var request violationStatusRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &request); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	status := strings.TrimSpace(request.Status)
	// Validated against the OPERATOR vocabulary, not the full one. active and
	// resolved are engine-owned: a caller asserting "resolved" would be
	// asserting a fact about the world rather than an intent, and the
	// compliance report would start lying about what is still true.
	if !domain.ValidOperatorPolicyStatus(status) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"status must be one of acknowledged, exempted")
		return
	}

	note, err := s.validatePolicyNote(request.Note)
	if err != nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}

	violation, err := s.policies.UpdateViolationStatus(r.Context(), id,
		domain.PolicyViolationStatus(status), note, s.now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "policy violation not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy violation status update failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.auditWrite(r, domain.AuditPolicyAnnotated, domain.AuditTargetViolation,
		strconv.FormatInt(violation.ID, 10), violation.ContainerName,
		"status set to "+string(violation.Status))

	writeJSON(w, r, s.logger, http.StatusOK, violation)
}

// handlePolicySummary returns the compliance aggregate.
func (s *Server) handlePolicySummary(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}

	summary, err := s.policies.PolicySummary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "policy summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, summary)
}

// policyContainerResponse is one container's compliance, with the pass that
// produced it.
//
// The evaluation is included so a client can tell "compliant" from "never
// checked" without a second request. Serving the violations alone would let a
// UI render an empty list as a clean bill of health for a container no pass has
// ever reached.
type policyContainerResponse struct {
	ContainerID string                   `json:"containerId"`
	Violations  []domain.PolicyViolation `json:"violations"`
	Pagination  Pagination               `json:"pagination"`
	// Evaluation is absent when the container has never been evaluated.
	Evaluation *domain.PolicyEvaluation `json:"evaluation,omitempty"`
}

// handlePolicyByContainer lists one container's violations.
func (s *Server) handlePolicyByContainer(w http.ResponseWriter, r *http.Request) {
	if s.policyUnavailable(w, r) {
		return
	}

	// Resolved through the container repository so a short id prefix works
	// here exactly as it does on every other container route, and so an
	// ambiguous prefix is a 409 rather than an arbitrary pick.
	containerID, ok := s.resolveContainer(w, r)
	if !ok {
		return
	}

	query, err := parseViolationQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	// The path segment is authoritative; a containerId query parameter cannot
	// widen the request to another container.
	query.ContainerID = containerID

	violations, total, err := s.policies.ListViolations(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "container policy list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	response := policyContainerResponse{
		ContainerID: containerID,
		Violations:  violations,
		Pagination:  newPagination(query.Page, query.PageSize, total),
	}

	evaluation, err := s.policies.PolicyEvaluation(r.Context(), containerID)
	switch {
	case err == nil:
		response.Evaluation = &evaluation
	case errors.Is(err, store.ErrNotFound):
		// Never evaluated. Left absent rather than invented, which is the
		// distinction this response exists to preserve.
	default:
		s.logger.ErrorContext(r.Context(), "policy evaluation load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, response)
}

// policyEvaluateResponse acknowledges a requested pass.
type policyEvaluateResponse struct {
	// Requested is always true on a 202: the endpoint schedules work and does
	// not wait for it.
	Requested bool                      `json:"requested"`
	Engine    domain.PolicyEngineStatus `json:"engine"`
}

// handlePolicyEvaluate requests a full compliance pass.
//
// ASYNCHRONOUS, and deliberately so. A synchronous evaluation of a
// thousand-container estate would hold an unauthenticated request open for
// minutes and give one caller a way to occupy a connection and the single
// SQLite writer at will. The request is coalesced by the same queue the
// scheduled passes use, so calling it in a loop produces one pass, not a
// backlog -- and it is rate limited on top of that.
func (s *Server) handlePolicyEvaluate(w http.ResponseWriter, r *http.Request) {
	if s.policyEngine == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the policy engine is not configured")
		return
	}

	s.policyEngine.RequestSweep()
	s.auditWrite(r, domain.AuditPolicyEvaluated, domain.AuditTargetPolicy, "", "",
		"compliance evaluation requested")

	writeJSON(w, r, s.logger, http.StatusAccepted, policyEvaluateResponse{
		Requested: true,
		Engine:    s.policyEngine.Status(),
	})
}

// requestPolicySweep asks the engine to re-evaluate after a definition change.
//
// Non-blocking and optional: a server wired without an engine still serves the
// definition endpoints, and the next scheduled pass picks the change up.
func (s *Server) requestPolicySweep() {
	if s.policyEngine != nil {
		s.policyEngine.RequestSweep()
	}
}

// ---------------------------------------------------------------- helpers --

// policyLimits resolves the definition bounds the API validates against.
func (s *Server) policyLimits() domain.PolicyLimits {
	return domain.PolicyLimits{
		MaxRules:            s.policyCfg.MaxRulesPerPolicy,
		MaxValuesPerRule:    s.policyCfg.MaxValuesPerRule,
		MaxNameBytes:        s.policyCfg.MaxNameBytes,
		MaxDescriptionBytes: s.policyCfg.MaxDescriptionBytes,
	}
}

// writePolicyValidationError renders a definition validation failure.
//
// The message names the field and the constraint. It never echoes the offending
// value: domain.PolicyValidationError is built without one, so there is nothing
// here that could reflect caller input even by accident.
func (s *Server) writePolicyValidationError(w http.ResponseWriter, r *http.Request, err error) {
	var validation domain.PolicyValidationError
	if errors.As(err, &validation) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, validation.Error())
		return
	}
	writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, "the policy is not valid")
}

// validatePolicyNote bounds and validates the operator's annotation.
//
// Length is measured in BYTES and validity in runes: a limit expressed in
// characters would let a caller send a megabyte of astral-plane text under a
// 500-"character" cap, and invalid UTF-8 would corrupt the column.
func (s *Server) validatePolicyNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", nil
	}

	limit := s.policyCfg.MaxNoteBytes
	if limit <= 0 {
		limit = 500
	}
	if err := validateText("note", trimmed, limit); err != nil {
		return "", err
	}
	return trimmed, nil
}

// policyID reads and validates the {id} path segment.
func (s *Server) policyID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	// Validated by SHAPE before it reaches a query. Server-generated ids have
	// exactly one form, so anything else is a miss and there is no reason to
	// let arbitrary caller text through to the database layer.
	if !validPolicyID(raw) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the policy id is not well formed")
		return "", false
	}
	return raw, true
}

// violationID reads and validates the {id} path segment.
func (s *Server) violationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the violation id must be a positive integer")
		return 0, false
	}
	return id, true
}
