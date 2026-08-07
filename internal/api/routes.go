package api

import (
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The route table.
//
// # This file is the authorization policy
//
// Every route HarborMaster serves is listed here exactly once, with the access
// policy it is served under. Nothing else in the package registers a handler,
// and the registration helpers all take a policy -- so a route without one
// cannot exist.
//
// Two tests enforce that:
//
//   - TestEveryRouteDeclaresAnAccessPolicy walks this table and fails on any
//     entry whose policy is the zero value.
//   - TestNoHandlerChecksARoleDirectly fails the build if any handler file
//     names a role constant, so an authorization decision cannot migrate out of
//     this file into a handler.
//
// # Reading an entry
//
//	{method, pattern, policy, handler}
//
// A `method` of "" registers the BARE pattern, which is how a known path
// answers 405 for an unsupported method rather than falling through to the
// "/api/" catch-all and answering 404. Those entries carry the same policy as
// the path's read route, so an unauthenticated caller gets 401 and learns
// nothing about which paths exist.

// route is one registration.
type route struct {
	// method is the HTTP method, or "" for the bare method-not-allowed entry.
	method string
	// pattern is the path, in net/http ServeMux syntax.
	pattern string
	// access is who may reach it. The zero value is invalid and fails a test.
	access routeAccess
	// handler serves it. Nil for a bare entry, which uses allowed instead.
	handler http.HandlerFunc
	// allowed is the Allow header for a bare entry.
	allowed string
}

// routeTable returns every route, in registration order.
//
// Long, and deliberately not abbreviated with loops or helpers that group
// several paths under one policy. A reader auditing "who can stop a container"
// should be able to find the answer by searching for the permission, and a
// grouping construct is exactly what hides one entry inside another's rule.
func (s *Server) routeTable() []route {
	p := APIPrefix

	return []route{
		// ---- public -------------------------------------------------------
		//
		// Four routes, and each one's reason is stated. This list is the whole
		// of HarborMaster's unauthenticated surface.

		// Liveness. Answering at all is the signal a container runtime needs,
		// and the body is REDUCED for an unauthenticated caller -- see
		// handleHealth. A probe cannot hold a session, so this cannot require
		// one.
		{http.MethodGet, p + "/health", public(), s.handleHealth, ""},
		{"", p + "/health", public(), nil, "GET, HEAD"},

		// Build metadata. Non-sensitive by construction: a version string, a
		// commit, a build date. Public so a deployment can be identified by
		// tooling that has no credential.
		{http.MethodGet, p + "/version", public(), s.handleVersion, ""},
		{"", p + "/version", public(), nil, "GET, HEAD"},

		// The login endpoint itself. CSRF-exempt because there is no session to
		// derive a token from; protected instead by SameSite, the Origin and
		// Fetch Metadata checks, the JSON content-type requirement, and the
		// rate limiter.
		{http.MethodPost, p + "/auth/login", public().noCSRF(), s.handleLogin, ""},
		{"", p + "/auth/login", public(), nil, "POST"},

		// Whether this installation has been claimed. Public so the SPA can
		// decide between the login page and the bootstrap page before it holds
		// anything. Reveals only that one bit.
		{http.MethodGet, p + "/auth/bootstrap", public(), s.handleBootstrapStatus, ""},
		// Claiming it. Reachable ONLY while unclaimed, and requires the
		// one-time token printed at startup.
		{http.MethodPost, p + "/auth/bootstrap", bootstrapOnly(), s.handleBootstrap, ""},
		{"", p + "/auth/bootstrap", public(), nil, "GET, HEAD, POST"},

		// ---- the authenticated session ------------------------------------
		//
		// These three are reachable by an account that must still change its
		// password, because otherwise it could not: it needs to read its own
		// session, change the password, and sign out.

		{http.MethodGet, p + "/auth/session", duringPasswordChange(), s.handleSession, ""},
		{"", p + "/auth/session", duringPasswordChange(), nil, "GET, HEAD"},

		{http.MethodPost, p + "/auth/logout", duringPasswordChange(), s.handleLogout, ""},
		{"", p + "/auth/logout", duringPasswordChange(), nil, "POST"},

		{http.MethodPost, p + "/auth/password", duringPasswordChange(), s.handleChangePassword, ""},
		{"", p + "/auth/password", duringPasswordChange(), nil, "POST"},

		// An operator's own sessions. No permission: everyone may see and end
		// their own. Ending SOMEBODY ELSE'S is user administration and lives
		// under /users.
		{http.MethodGet, p + "/auth/sessions", authenticated(), s.handleListSessions, ""},
		{"", p + "/auth/sessions", authenticated(), nil, "GET, HEAD"},

		{http.MethodPost, p + "/auth/sessions/{id}/revoke", authenticated(), s.handleRevokeSession, ""},
		{"", p + "/auth/sessions/{id}/revoke", authenticated(), nil, "POST"},

		// ---- inventory ----------------------------------------------------

		{http.MethodGet, p + "/inventory", requires(domain.PermInventoryRead), s.handleInventory, ""},
		{"", p + "/inventory", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/inventory/filters", requires(domain.PermInventoryRead), s.handleContainerFilters, ""},
		{"", p + "/inventory/filters", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodPost, p + "/inventory/refresh", requires(domain.PermInventoryRefresh), s.handleInventoryRefresh, ""},
		{"", p + "/inventory/refresh", requires(domain.PermInventoryRefresh), nil, "POST"},

		{http.MethodGet, p + "/containers", requires(domain.PermInventoryRead), s.handleContainers, ""},
		{"", p + "/containers", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/containers/{id}", requires(domain.PermInventoryRead), s.handleContainerDetail, ""},
		{"", p + "/containers/{id}", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/containers/{id}/raw", requires(domain.PermInventoryRead), s.handleContainerRaw, ""},
		{"", p + "/containers/{id}/raw", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/images", requires(domain.PermInventoryRead), s.handleImages, ""},
		{"", p + "/images", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		// GET-only, with no bare companion: a bare "/images/updates" would
		// match every method on that literal while "GET /images/{id}" matches
		// GET on every segment, and ServeMux rejects the ambiguous pair. A
		// non-GET request still gets an honest 405 from "/images/{id}".
		{http.MethodGet, p + "/images/updates", requires(domain.PermInventoryRead), s.handleImageUpdates, ""},
		// Strictly more specific than "/images/{id}", so this pair resolves.
		{http.MethodPost, p + "/images/refresh", requires(domain.PermImageRefresh), s.handleImageRefresh, ""},

		{http.MethodGet, p + "/images/{id}", requires(domain.PermInventoryRead), s.handleImageDetail, ""},
		{"", p + "/images/{id}", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/images/{id}/history", requires(domain.PermInventoryRead), s.handleImageHistory, ""},
		{"", p + "/images/{id}/history", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/networks", requires(domain.PermInventoryRead), s.handleNetworks, ""},
		{"", p + "/networks", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/volumes", requires(domain.PermInventoryRead), s.handleVolumes, ""},
		{"", p + "/volumes", requires(domain.PermInventoryRead), nil, "GET, HEAD"},

		// ---- events -------------------------------------------------------

		{http.MethodGet, p + "/events", requires(domain.PermEventRead), s.handleEvents, ""},
		{"", p + "/events", requires(domain.PermEventRead), nil, "GET, HEAD"},

		// GET-only for the same ServeMux reason as /images/updates.
		{http.MethodGet, p + "/events/stream", requires(domain.PermEventRead), s.handleEventStream, ""},

		{http.MethodGet, p + "/events/{id}", requires(domain.PermEventRead), s.handleEventDetail, ""},
		{"", p + "/events/{id}", requires(domain.PermEventRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/event-engine", requires(domain.PermEventRead), s.handleEventEngine, ""},
		{"", p + "/event-engine", requires(domain.PermEventRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/event-filters", requires(domain.PermEventRead), s.handleEventFilters, ""},
		{"", p + "/event-filters", requires(domain.PermEventRead), nil, "GET, HEAD"},

		// ---- snapshots ----------------------------------------------------

		{http.MethodGet, p + "/snapshots", requires(domain.PermSnapshotRead), s.handleSnapshots, ""},
		{http.MethodPost, p + "/snapshots", requires(domain.PermSnapshotCreate), s.handleSnapshotCreate, ""},
		{"", p + "/snapshots", requires(domain.PermSnapshotRead), nil, "GET, HEAD, POST"},

		{http.MethodGet, p + "/snapshots/{id}", requires(domain.PermSnapshotRead), s.handleSnapshotDetail, ""},
		{"", p + "/snapshots/{id}", requires(domain.PermSnapshotRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/snapshots/{id}/diff", requires(domain.PermSnapshotRead), s.handleSnapshotDiff, ""},
		{"", p + "/snapshots/{id}/diff", requires(domain.PermSnapshotRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/snapshots/{id}/restore-readiness", requires(domain.PermSnapshotRead), s.handleSnapshotReadiness, ""},
		{"", p + "/snapshots/{id}/restore-readiness", requires(domain.PermSnapshotRead), nil, "GET, HEAD"},

		// ---- drift --------------------------------------------------------

		{http.MethodGet, p + "/drift", requires(domain.PermDriftRead), s.handleDrift, ""},
		{"", p + "/drift", requires(domain.PermDriftRead), nil, "GET, HEAD"},

		// GET-only for the ServeMux reason.
		{http.MethodGet, p + "/drift/summary", requires(domain.PermDriftRead), s.handleDriftSummary, ""},

		{http.MethodGet, p + "/drift/container/{id}", requires(domain.PermDriftRead), s.handleDriftByContainer, ""},
		{"", p + "/drift/container/{id}", requires(domain.PermDriftRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/drift/{id}", requires(domain.PermDriftRead), s.handleDriftDetail, ""},
		{http.MethodPatch, p + "/drift/{id}", requires(domain.PermDriftAnnotate), s.handleDriftPatch, ""},
		{"", p + "/drift/{id}", requires(domain.PermDriftRead), nil, "GET, HEAD, PATCH"},

		// ---- policy -------------------------------------------------------
		//
		// Note the split: reading and ANNOTATING are operator-level, while
		// creating, editing, and withdrawing a policy are ADMINISTRATOR-level.
		// A policy is what blocks an acquisition or a recreation, so an operator
		// able to edit one could remove the gate standing in their way.

		{http.MethodGet, p + "/policies", requires(domain.PermPolicyRead), s.handlePolicies, ""},
		{http.MethodPost, p + "/policies", requires(domain.PermPolicyManage).policyBudget(), s.handlePolicyCreate, ""},
		{"", p + "/policies", requires(domain.PermPolicyRead), nil, "GET, HEAD, POST"},

		{http.MethodGet, p + "/policies/{id}", requires(domain.PermPolicyRead), s.handlePolicyDetail, ""},
		{http.MethodPatch, p + "/policies/{id}", requires(domain.PermPolicyManage).policyBudget(), s.handlePolicyUpdate, ""},
		{http.MethodDelete, p + "/policies/{id}", requires(domain.PermPolicyManage).policyBudget(), s.handlePolicyDelete, ""},
		{"", p + "/policies/{id}", requires(domain.PermPolicyRead), nil, "GET, HEAD, PATCH, DELETE"},

		{http.MethodGet, p + "/policy-rules", requires(domain.PermPolicyRead), s.handlePolicyRules, ""},
		{"", p + "/policy-rules", requires(domain.PermPolicyRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/policy-summary", requires(domain.PermPolicyRead), s.handlePolicySummary, ""},
		{"", p + "/policy-summary", requires(domain.PermPolicyRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/policy-violations", requires(domain.PermPolicyRead), s.handlePolicyViolations, ""},
		{"", p + "/policy-violations", requires(domain.PermPolicyRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/policy-violations/container/{id}", requires(domain.PermPolicyRead), s.handlePolicyByContainer, ""},
		{"", p + "/policy-violations/container/{id}", requires(domain.PermPolicyRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/policy-violations/{id}", requires(domain.PermPolicyRead), s.handlePolicyViolationDetail, ""},
		{http.MethodPatch, p + "/policy-violations/{id}", requires(domain.PermPolicyAnnotate).policyBudget(), s.handlePolicyViolationPatch, ""},
		{"", p + "/policy-violations/{id}", requires(domain.PermPolicyRead), nil, "GET, HEAD, PATCH"},

		{http.MethodPost, p + "/policy/evaluate", requires(domain.PermPolicyEvaluate).policyBudget(), s.handlePolicyEvaluate, ""},
		{"", p + "/policy/evaluate", requires(domain.PermPolicyEvaluate), nil, "POST"},

		// ---- plans --------------------------------------------------------

		{http.MethodGet, p + "/plans", requires(domain.PermPlanRead), s.handlePlans, ""},
		{"", p + "/plans", requires(domain.PermPlanRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/plans/container/{id}", requires(domain.PermPlanRead), s.handlePlansByContainer, ""},
		{"", p + "/plans/container/{id}", requires(domain.PermPlanRead), nil, "GET, HEAD"},

		// Strictly more specific than "/plans/{id}", so this pair resolves.
		{http.MethodPost, p + "/plans/generate", requires(domain.PermPlanGenerate), s.handlePlanGenerate, ""},

		{http.MethodGet, p + "/plans/{id}", requires(domain.PermPlanRead), s.handlePlanDetail, ""},
		{"", p + "/plans/{id}", requires(domain.PermPlanRead), nil, "GET, HEAD"},

		// ---- image acquisition --------------------------------------------
		//
		// The first of the two permissions that reach a privileged socket.

		{http.MethodGet, p + "/acquisitions", requires(domain.PermAcquisitionRead), s.handleAcquisitions, ""},
		{http.MethodPost, p + "/acquisitions", requires(domain.PermAcquisitionCreate), s.handleAcquisitionCreate, ""},
		{"", p + "/acquisitions", requires(domain.PermAcquisitionRead), nil, "GET, HEAD, POST"},

		{http.MethodPost, p + "/acquisitions/{id}/cancel", requires(domain.PermAcquisitionCancel), s.handleAcquisitionCancel, ""},
		{"", p + "/acquisitions/{id}/cancel", requires(domain.PermAcquisitionCancel), nil, "POST"},

		{http.MethodGet, p + "/acquisitions/{id}", requires(domain.PermAcquisitionRead), s.handleAcquisitionDetail, ""},
		{"", p + "/acquisitions/{id}", requires(domain.PermAcquisitionRead), nil, "GET, HEAD"},

		// ---- container recreation ------------------------------------------
		//
		// THE LARGEST PERMISSION IN HARBORMASTER. PermExecutionCreate stops a
		// running container and replaces it.

		{http.MethodGet, p + "/executions", requires(domain.PermExecutionRead), s.handleExecutions, ""},
		{http.MethodPost, p + "/executions", requires(domain.PermExecutionCreate), s.handleExecutionCreate, ""},
		{"", p + "/executions", requires(domain.PermExecutionRead), nil, "GET, HEAD, POST"},

		{http.MethodPost, p + "/executions/{id}/cancel", requires(domain.PermExecutionCancel), s.handleExecutionCancel, ""},
		{"", p + "/executions/{id}/cancel", requires(domain.PermExecutionCancel), nil, "POST"},

		{http.MethodGet, p + "/executions/{id}", requires(domain.PermExecutionRead), s.handleExecutionDetail, ""},
		{"", p + "/executions/{id}", requires(domain.PermExecutionRead), nil, "GET, HEAD"},

		// ---- manual rollback ------------------------------------------------
		//
		// THE OTHER PERMISSION THAT INTERRUPTS A SERVICE. PermRollbackCreate
		// stops the container a recreation put in place and starts the one it
		// replaced.
		//
		// Operator-level, like recreation. The person who has to undo a bad
		// recreation is the person who performed it, and requiring an
		// administrator would make the safest response the slowest one.

		{http.MethodGet, p + "/rollbacks", requires(domain.PermRollbackRead), s.handleRollbacks, ""},
		{http.MethodPost, p + "/rollbacks", requires(domain.PermRollbackCreate), s.handleRollbackCreate, ""},
		{"", p + "/rollbacks", requires(domain.PermRollbackRead), nil, "GET, HEAD, POST"},

		{http.MethodPost, p + "/rollbacks/{id}/cancel", requires(domain.PermRollbackCancel), s.handleRollbackCancel, ""},
		{"", p + "/rollbacks/{id}/cancel", requires(domain.PermRollbackCancel), nil, "POST"},

		{http.MethodGet, p + "/rollbacks/{id}", requires(domain.PermRollbackRead), s.handleRollbackDetail, ""},
		{"", p + "/rollbacks/{id}", requires(domain.PermRollbackRead), nil, "GET, HEAD"},

		// ---- update policies -------------------------------------------------
		//
		// THE MOST CONSEQUENTIAL WRITES IN THIS TABLE. An update policy is a
		// standing instruction that lets HarborMaster stop and replace
		// containers a selector reaches, with nobody watching.
		//
		// So writing one is `automation:manage`, which only an ADMINISTRATOR
		// holds -- for the same reason `policy:manage` is: an operator able to
		// write one would be granting themselves a permanent, unattended
		// version of `execution:create` over any container the selector
		// reaches.
		//
		// Reading one is `automation:read`, which every role holds. Automation
		// changes the host without being asked; a viewer who cannot see the
		// rules cannot answer the question their role exists for.
		//
		// The writes share the POLICY rate budget rather than the shared one:
		// they cost one small transaction on HarborMaster's own table, not a
		// Docker sweep.

		{http.MethodGet, p + "/update-policies", requires(domain.PermAutomationRead), s.handleUpdatePolicies, ""},
		{http.MethodPost, p + "/update-policies", requires(domain.PermAutomationManage).policyBudget(), s.handleUpdatePolicyCreate, ""},
		{"", p + "/update-policies", requires(domain.PermAutomationRead), nil, "GET, HEAD, POST"},

		{http.MethodGet, p + "/update-policies/{id}", requires(domain.PermAutomationRead), s.handleUpdatePolicyDetail, ""},
		{http.MethodPatch, p + "/update-policies/{id}", requires(domain.PermAutomationManage).policyBudget(), s.handleUpdatePolicyUpdate, ""},
		// DELETE archives. Decisions and pauses reference the row, and the
		// record of what automation did must outlive the rule being withdrawn.
		{http.MethodDelete, p + "/update-policies/{id}", requires(domain.PermAutomationManage).policyBudget(), s.handleUpdatePolicyDelete, ""},
		{"", p + "/update-policies/{id}", requires(domain.PermAutomationRead), nil, "GET, HEAD, PATCH, DELETE"},

		// ---- the automation engine -------------------------------------------
		//
		// Running a pass is `automation:run`, an OPERATOR permission: a manual
		// pass submits exactly the work the scheduled pass would have submitted
		// a few minutes later, so it changes WHEN, not WHETHER.
		//
		// Approving a held decision is its own permission, and it is marked
		// privileged: the approver releases a change that will stop and replace
		// a running container.
		//
		// Pausing and resuming share one permission. Pausing is a safety action
		// anybody operating the host should be able to take immediately, and
		// requiring an administrator to undo it would make the safe state the
		// inconvenient one.

		{http.MethodGet, p + "/automation", requires(domain.PermAutomationRead), s.handleAutomationStatus, ""},
		{"", p + "/automation", requires(domain.PermAutomationRead), nil, "GET, HEAD"},

		// A read that previews the next pass WITHOUT running it: no run row, no
		// decisions, no request to any service.
		{http.MethodGet, p + "/automation/upcoming", requires(domain.PermAutomationRead), s.handleAutomationUpcoming, ""},
		{"", p + "/automation/upcoming", requires(domain.PermAutomationRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/automation/runs", requires(domain.PermAutomationRead), s.handleAutomationRuns, ""},
		{"", p + "/automation/runs", requires(domain.PermAutomationRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/automation/runs/{id}", requires(domain.PermAutomationRead), s.handleAutomationRunDetail, ""},
		{"", p + "/automation/runs/{id}", requires(domain.PermAutomationRead), nil, "GET, HEAD"},

		{http.MethodGet, p + "/automation/paused", requires(domain.PermAutomationRead), s.handleAutomationPauses, ""},
		{"", p + "/automation/paused", requires(domain.PermAutomationRead), nil, "GET, HEAD"},

		{http.MethodPost, p + "/automation/run", requires(domain.PermAutomationRun).policyBudget(), s.handleAutomationRun, ""},
		{"", p + "/automation/run", requires(domain.PermAutomationRun), nil, "POST"},

		{http.MethodPost, p + "/automation/approve", requires(domain.PermAutomationApprove), s.handleAutomationApprove, ""},
		{"", p + "/automation/approve", requires(domain.PermAutomationApprove), nil, "POST"},

		{http.MethodPost, p + "/automation/pause", requires(domain.PermAutomationPause).policyBudget(), s.handleAutomationPause, ""},
		{"", p + "/automation/pause", requires(domain.PermAutomationPause), nil, "POST"},

		{http.MethodPost, p + "/automation/resume", requires(domain.PermAutomationPause).policyBudget(), s.handleAutomationResume, ""},
		{"", p + "/automation/resume", requires(domain.PermAutomationPause), nil, "POST"},

		// ---- user administration -------------------------------------------

		{http.MethodGet, p + "/users", requires(domain.PermUserManage), s.handleUsers, ""},
		{http.MethodPost, p + "/users", requires(domain.PermUserManage), s.handleUserCreate, ""},
		{"", p + "/users", requires(domain.PermUserManage), nil, "GET, HEAD, POST"},

		// Three segments, so this never overlaps the two-segment "/users/{id}".
		{http.MethodPost, p + "/users/{id}/password-reset", requires(domain.PermUserManage), s.handleUserPasswordReset, ""},
		{"", p + "/users/{id}/password-reset", requires(domain.PermUserManage), nil, "POST"},

		{http.MethodGet, p + "/users/{id}", requires(domain.PermUserManage), s.handleUserDetail, ""},
		// PATCH covers both the role and the status. There is deliberately no
		// DELETE: disabling preserves the history an audit record depends on.
		{http.MethodPatch, p + "/users/{id}", requires(domain.PermUserManage), s.handleUserUpdate, ""},
		{"", p + "/users/{id}", requires(domain.PermUserManage), nil, "GET, HEAD, PATCH"},

		// The role catalogue, so the role picker is built from the same source
		// of truth the authorization middleware uses.
		{http.MethodGet, p + "/roles", requires(domain.PermUserManage), s.handleRoles, ""},
		{"", p + "/roles", requires(domain.PermUserManage), nil, "GET, HEAD"},

		// ---- the security audit log -----------------------------------------

		{http.MethodGet, p + "/audit", requires(domain.PermAuditRead), s.handleAudit, ""},
		{"", p + "/audit", requires(domain.PermAuditRead), nil, "GET, HEAD"},

		// GET-only for the ServeMux reason: "/audit/summary" as a bare literal
		// would neither contain nor be contained by any pattern above.
		{http.MethodGet, p + "/audit/summary", requires(domain.PermAuditRead), s.handleAuditSummary, ""},
	}
}
