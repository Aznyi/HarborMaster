package domain

import (
	"crypto/rand"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Security audit events.
//
// # What this is for
//
// Every operational feature in HarborMaster already records what happened: a
// snapshot row, an acquisition record, an execution record. What none of them
// recorded until now is WHO. The audit log answers that, and answers it for the
// actions where the answer matters -- authentication, authorization failures,
// user administration, and every write that changes the host.
//
// # Immutable
//
// Rows are inserted and never updated. There is no repository method that
// modifies one, no endpoint that edits one, and a test asserts the package
// contains no UPDATE against the table. An audit trail that can be edited is
// not an audit trail.
//
// # What must never appear here
//
// Passwords, password hashes, session tokens, CSRF tokens, environment values,
// registry credentials, raw request bodies, and any Docker data that could
// carry a secret. The type has nowhere to put them: every field is either a
// closed vocabulary, an identifier, or a bounded string built from
// HarborMaster's own words. A test puts a known secret through every field and
// asserts it cannot be stored.

// AuditAction names what happened.
//
// A closed vocabulary. The alternative -- free-form action strings -- makes the
// log unqueryable and lets a caller write whatever it likes into a column an
// administrator reads.
type AuditAction string

// Authentication actions.
const (
	AuditLoginSucceeded AuditAction = "auth.login.succeeded"
	// AuditLoginFailed is recorded for a wrong password AND for an unknown
	// username, with the same shape, so the LOG does not become the
	// enumeration oracle the login response refuses to be.
	AuditLoginFailed         AuditAction = "auth.login.failed"
	AuditLoginRateLimited    AuditAction = "auth.login.rateLimited"
	AuditLogout              AuditAction = "auth.logout"
	AuditSessionExpired      AuditAction = "auth.session.expired"
	AuditSessionRevoked      AuditAction = "auth.session.revoked"
	AuditPasswordChanged     AuditAction = "auth.password.changed"
	AuditPasswordReset       AuditAction = "auth.password.reset"
	AuditCSRFRejected        AuditAction = "auth.csrf.rejected"
	AuditAuthorizationDenied AuditAction = "auth.authorization.denied"
)

// Bootstrap actions.
const (
	AuditBootstrapStarted   AuditAction = "bootstrap.started"
	AuditBootstrapCompleted AuditAction = "bootstrap.completed"
	AuditBootstrapRejected  AuditAction = "bootstrap.rejected"
)

// User administration actions.
const (
	AuditUserCreated     AuditAction = "user.created"
	AuditUserRoleChanged AuditAction = "user.roleChanged"
	AuditUserDisabled    AuditAction = "user.disabled"
	AuditUserEnabled     AuditAction = "user.enabled"
)

// Operational actions. Every write path in HarborMaster appears here, so
// "who asked for this" is answerable for all of them.
const (
	AuditInventoryRefreshed AuditAction = "inventory.refreshed"
	AuditSnapshotCreated    AuditAction = "snapshot.created"
	AuditDriftAnnotated     AuditAction = "drift.annotated"
	AuditPolicyCreated      AuditAction = "policy.created"
	AuditPolicyUpdated      AuditAction = "policy.updated"
	AuditPolicyArchived     AuditAction = "policy.archived"
	AuditPolicyEvaluated    AuditAction = "policy.evaluated"
	AuditPolicyAnnotated    AuditAction = "policy.annotated"
	AuditImageRefreshed     AuditAction = "image.refreshed"
	AuditPlanGenerated      AuditAction = "plan.generated"

	// The two that change the Docker host.
	//
	// Each has a REQUEST and an OUTCOME, and they are separate events because
	// they are separate facts. A request can be refused by the second
	// preflight, cancelled before anything moves, expire in the queue, or fail
	// partway. "Somebody asked" and "the host changed" must not be the same row.
	AuditAcquisitionRequested AuditAction = "acquisition.requested"
	AuditAcquisitionCancelled AuditAction = "acquisition.cancelled"
	// AuditAcquisitionCompleted means an image is now in the local store.
	AuditAcquisitionCompleted AuditAction = "acquisition.completed"
	// AuditAcquisitionFailed means it is not, and says why.
	AuditAcquisitionFailed AuditAction = "acquisition.failed"

	AuditExecutionRequested AuditAction = "execution.requested"
	AuditExecutionCancelled AuditAction = "execution.cancelled"
	// AuditExecutionCompleted means a container was replaced and the original
	// removed. This is the most consequential row HarborMaster writes.
	AuditExecutionCompleted AuditAction = "execution.completed"
	// AuditExecutionFailed means the recreation did not finish. The reason says
	// whether the host was left changed, because that is what decides whether
	// somebody has to go and look.
	AuditExecutionFailed AuditAction = "execution.failed"

	// Manual rollback: returning a container to the state a recreation moved it
	// from. The same request/outcome split, for the same reason.
	AuditRollbackRequested AuditAction = "rollback.requested"
	AuditRollbackCancelled AuditAction = "rollback.cancelled"
	// AuditRollbackCompleted means a container was returned to its preserved
	// original and that original is running and proved.
	AuditRollbackCompleted AuditAction = "rollback.completed"
	// AuditRollbackFailed means it did not finish. The reason says whether the
	// host was left changed -- and for a rollback that matters more than
	// anywhere else, because a failure partway can leave nothing serving.
	AuditRollbackFailed AuditAction = "rollback.failed"
)

// Automation actions.
//
// # Why an automated change is audited more, not less
//
// Every row above answers "who asked". Automation is the one path where the
// answer is "nobody did", and that makes the trail more important rather than
// less: the only account of why the host changed at 02:14 is the one
// HarborMaster wrote at 02:14.
//
// So automation records the administration of the policies (who created the
// rule that later acted), the passes (what the engine did and when), and the
// safety interventions (what it refused to do again, and who cleared that).
// The mutations themselves are NOT re-audited here -- they are recorded by the
// acquisition, execution, and rollback actions above, because the automation
// engine submits exactly the same requests a human would and reaches exactly
// the same audited code path. Duplicating them would make the host-change
// counter over-report the very thing it exists to count.
const (
	AuditUpdatePolicyCreated  AuditAction = "updatePolicy.created"
	AuditUpdatePolicyUpdated  AuditAction = "updatePolicy.updated"
	AuditUpdatePolicyArchived AuditAction = "updatePolicy.archived"

	// AuditAutomationRunStarted is written for a pass a person asked for. A
	// scheduled pass writes its run row and is not audited as a request,
	// because a request is something somebody made.
	AuditAutomationRunStarted AuditAction = "automation.run.started"
	// AuditAutomationRunCompleted records the counts a pass finished with.
	AuditAutomationRunCompleted AuditAction = "automation.run.completed"
	// AuditAutomationRunFailed records a pass that could not complete.
	AuditAutomationRunFailed AuditAction = "automation.run.failed"

	// AuditAutomationApproved is a person releasing a decision the engine made
	// but was not permitted to act on.
	// Notification administration.
	//
	// A destination carries a CREDENTIAL and points HarborMaster's second
	// outbound egress somewhere. Creating, editing, and archiving one are all
	// audited, and a test send is audited too: it is a real outbound request
	// somebody caused, and "who made this host talk to that server" is exactly
	// the question the audit log exists to answer.
	//
	// No audit record carries the URL. The destination NAME and its
	// identifier are what a reader needs, and the name is one an administrator
	// chose rather than a token.
	AuditNotificationDestinationCreated  AuditAction = "notificationDestination.created"
	AuditNotificationDestinationUpdated  AuditAction = "notificationDestination.updated"
	AuditNotificationDestinationArchived AuditAction = "notificationDestination.archived"
	AuditNotificationDestinationTested   AuditAction = "notificationDestination.tested"

	AuditNotificationRuleCreated  AuditAction = "notificationRule.created"
	AuditNotificationRuleUpdated  AuditAction = "notificationRule.updated"
	AuditNotificationRuleArchived AuditAction = "notificationRule.archived"

	AuditAutomationApproved AuditAction = "automation.approved"
	AuditAutomationRejected AuditAction = "automation.rejected"

	// AuditAutomationPaused is HarborMaster refusing to keep trying. Recorded
	// as an event in its own right: an operator must be able to find the moment
	// automation stopped touching a container without reading every run.
	AuditAutomationPaused AuditAction = "automation.paused"
	// AuditAutomationResumed is a person clearing that refusal.
	AuditAutomationResumed AuditAction = "automation.resumed"

	// Workload dependencies.
	//
	// # Four actions, and why DISCOVERY is not one of them
	//
	// There is deliberately no `dependency.detected`. A discovered relationship
	// is derived from the inventory on every read, so an action per detected
	// edge per refresh would be thousands of rows a day describing that nothing
	// changed -- and an audit log nobody can read is an audit log nobody reads.
	//
	// What IS recorded is the four things a person did, or that changed what
	// HarborMaster will do:

	// AuditDependencyCreated is an operator recording an ordering constraint.
	AuditDependencyCreated AuditAction = "dependency.created"
	// AuditDependencyDeleted is an operator REMOVING one, which takes a safety
	// constraint away and is why the permission is administrator-only.
	AuditDependencyDeleted AuditAction = "dependency.deleted"
	// AuditDependencyBlocked is HarborMaster refusing to advance a container
	// because of what it depends on.
	//
	// Recorded once per refusal, not once per evaluation: a decision pass that
	// declines the same container every fifteen minutes writes one row when the
	// answer CHANGES, never one per pass.
	AuditDependencyBlocked AuditAction = "dependency.blocked"
	// AuditDependencyRebindRequired is HarborMaster establishing that a
	// container must be reattached to a replaced namespace.
	AuditDependencyRebindRequired AuditAction = "dependency.rebindRequired"
)

// AuditActions lists every action.
var AuditActions = []AuditAction{
	AuditLoginSucceeded, AuditLoginFailed, AuditLoginRateLimited, AuditLogout,
	AuditSessionExpired, AuditSessionRevoked,
	AuditPasswordChanged, AuditPasswordReset,
	AuditCSRFRejected, AuditAuthorizationDenied,

	AuditBootstrapStarted, AuditBootstrapCompleted, AuditBootstrapRejected,

	AuditUserCreated, AuditUserRoleChanged, AuditUserDisabled, AuditUserEnabled,

	AuditInventoryRefreshed, AuditSnapshotCreated, AuditDriftAnnotated,
	AuditPolicyCreated, AuditPolicyUpdated, AuditPolicyArchived,
	AuditPolicyEvaluated, AuditPolicyAnnotated,
	AuditImageRefreshed, AuditPlanGenerated,
	AuditAcquisitionRequested, AuditAcquisitionCancelled,
	AuditAcquisitionCompleted, AuditAcquisitionFailed,
	AuditExecutionRequested, AuditExecutionCancelled,
	AuditExecutionCompleted, AuditExecutionFailed,
	AuditRollbackRequested, AuditRollbackCancelled,
	AuditRollbackCompleted, AuditRollbackFailed,

	AuditUpdatePolicyCreated, AuditUpdatePolicyUpdated, AuditUpdatePolicyArchived,
	AuditAutomationRunStarted, AuditAutomationRunCompleted, AuditAutomationRunFailed,
	AuditAutomationApproved, AuditAutomationRejected,
	AuditAutomationPaused, AuditAutomationResumed,

	AuditDependencyCreated, AuditDependencyDeleted,
	AuditDependencyBlocked, AuditDependencyRebindRequired,

	AuditNotificationDestinationCreated, AuditNotificationDestinationUpdated,
	AuditNotificationDestinationArchived, AuditNotificationDestinationTested,
	AuditNotificationRuleCreated, AuditNotificationRuleUpdated,
	AuditNotificationRuleArchived,
}

// ValidAuditAction reports whether name is a known action.
func ValidAuditAction(name string) bool {
	for _, action := range AuditActions {
		if string(action) == name {
			return true
		}
	}
	return false
}

// Security reports whether the action belongs to the security surface.
//
// Used to decide what the audit page shows by default: an administrator opening
// it is usually asking about authentication and authorization rather than about
// yesterday's inventory refreshes.
func (a AuditAction) Security() bool {
	return strings.HasPrefix(string(a), "auth.") ||
		strings.HasPrefix(string(a), "user.") ||
		strings.HasPrefix(string(a), "bootstrap.")
}

// Privileged reports whether the action changed the Docker host.
// Privileged reports whether an action CHANGED the Docker host.
//
// Deliberately the completions, not the requests. A request is an intention and
// may come to nothing; a completion is a host that is different from how it was.
// Counting requests would make the summary's "host changes" number an
// over-count, and a security counter that over-reports is one an administrator
// learns to ignore.
//
// A privileged action is also logged at WARN, so it appears in a default log
// configuration without anybody opening a page.
func (a AuditAction) Privileged() bool {
	switch a {
	case AuditAcquisitionCompleted, AuditExecutionCompleted, AuditRollbackCompleted:
		return true
	default:
		return false
	}
}

// AuditOutcome is whether the action succeeded.
type AuditOutcome string

// Outcomes.
const (
	AuditSucceeded AuditOutcome = "succeeded"
	// AuditFailed means HarborMaster tried and could not.
	AuditFailed AuditOutcome = "failed"
	// AuditDenied means HarborMaster refused. Distinct from failed: a denial
	// is the security model working, and an administrator scanning for
	// problems wants to tell the two apart at a glance.
	AuditDenied AuditOutcome = "denied"
)

// AuditOutcomes lists every outcome.
var AuditOutcomes = []AuditOutcome{AuditSucceeded, AuditFailed, AuditDenied}

// ValidAuditOutcome reports whether name is a known outcome.
func ValidAuditOutcome(name string) bool {
	for _, outcome := range AuditOutcomes {
		if string(outcome) == name {
			return true
		}
	}
	return false
}

// AuditTargetType names what an action acted on.
//
// A closed vocabulary paired with a SAFE identifier -- one HarborMaster
// generated or validated, never free text.
type AuditTargetType string

// Target types.
const (
	AuditTargetNone        AuditTargetType = ""
	AuditTargetUser        AuditTargetType = "user"
	AuditTargetSession     AuditTargetType = "session"
	AuditTargetContainer   AuditTargetType = "container"
	AuditTargetSnapshot    AuditTargetType = "snapshot"
	AuditTargetDrift       AuditTargetType = "drift"
	AuditTargetPolicy      AuditTargetType = "policy"
	AuditTargetViolation   AuditTargetType = "violation"
	AuditTargetPlan        AuditTargetType = "plan"
	AuditTargetAcquisition AuditTargetType = "acquisition"
	AuditTargetExecution   AuditTargetType = "execution"
	AuditTargetRollback    AuditTargetType = "rollback"
	// AuditTargetUpdatePolicy is an automation rule, and AuditTargetAutomation
	// is a pass, a pause, or an approval. Distinct from AuditTargetPolicy,
	// which is a compliance rule: the two subsystems are separate on purpose
	// and an audit page that conflated them would report a reporting rule and
	// a mutation rule under one word.
	AuditTargetUpdatePolicy AuditTargetType = "updatePolicy"
	AuditTargetAutomation   AuditTargetType = "automation"
	// AuditTargetNotificationDestination is somewhere notifications are sent,
	// and AuditTargetNotificationRule is what routes them there. Separate,
	// because archiving a destination and archiving a rule have different
	// consequences and a reader must not have to guess which happened.
	AuditTargetNotificationDestination AuditTargetType = "notificationDestination"
	AuditTargetNotificationRule        AuditTargetType = "notificationRule"
	// AuditTargetDependency is a workload ordering relationship. Distinct from
	// AuditTargetContainer: the subject of the record is the RELATIONSHIP, and
	// a reader looking for "what stopped being enforced" must not have to infer
	// it from a container row.
	AuditTargetDependency AuditTargetType = "dependency"
	AuditTargetInventory  AuditTargetType = "inventory"
	AuditTargetSystem     AuditTargetType = "system"
)

// AuditTargetTypes lists every target type.
var AuditTargetTypes = []AuditTargetType{
	AuditTargetUser, AuditTargetSession, AuditTargetContainer,
	AuditTargetSnapshot, AuditTargetDrift, AuditTargetPolicy,
	AuditTargetViolation, AuditTargetPlan, AuditTargetAcquisition,
	AuditTargetExecution, AuditTargetRollback,
	AuditTargetUpdatePolicy, AuditTargetAutomation,
	AuditTargetNotificationDestination, AuditTargetNotificationRule,
	AuditTargetDependency,
	AuditTargetInventory, AuditTargetSystem,
}

// ValidAuditTargetType reports whether name is a known target type.
func ValidAuditTargetType(name string) bool {
	if name == "" {
		return true
	}
	for _, target := range AuditTargetTypes {
		if string(target) == name {
			return true
		}
	}
	return false
}

// Bounds on the free-ish text fields.
//
// Every one of them is written by HarborMaster from a fixed vocabulary, and
// bounded anyway: a bound that is never reached costs nothing, and one that is
// missing is discovered by the row that reaches it.
const (
	MaxAuditTargetIDBytes = 128
	MaxAuditReasonBytes   = 200
	MaxAuditActorBytes    = MaxUsernameBytes
	MaxAuditAddrBytes     = 64
)

// AuditEvent is one immutable security record.
type AuditEvent struct {
	ID int64 `json:"-"`
	// EventID is the public identifier.
	EventID string `json:"eventId"`

	Action  AuditAction  `json:"action"`
	Outcome AuditOutcome `json:"outcome"`

	// ActorUserID is the account that acted, empty for an unauthenticated
	// attempt. ActorUsername and ActorRole are SNAPSHOTS: an account renamed or
	// demoted later must not rewrite the history of what it did.
	ActorUserID   string `json:"actorUserId,omitempty"`
	ActorUsername string `json:"actorUsername,omitempty"`
	ActorRole     Role   `json:"actorRole,omitempty"`
	// ActorSessionID ties the action to a session, so revoking one and seeing
	// what it did are the same investigation.
	ActorSessionID string `json:"actorSessionId,omitempty"`

	TargetType AuditTargetType `json:"targetType,omitempty"`
	// TargetID is a HarborMaster-generated or validated identifier. Never free
	// text, and never a value that could carry a secret.
	TargetID string `json:"targetId,omitempty"`
	// TargetName is a bounded display name, sanitised. Present so an audit page
	// is readable without a join against a table whose row may since have been
	// pruned.
	TargetName string `json:"targetName,omitempty"`

	// RequestID correlates the audit row with the access log.
	RequestID string `json:"requestId,omitempty"`
	// ClientAddr is the normalised source address. See NormaliseClientAddr.
	ClientAddr string `json:"clientAddr,omitempty"`

	// Reason explains a non-success, from a fixed vocabulary. NEVER a driver
	// error, a daemon string, or anything derived from the request body.
	Reason string `json:"reason,omitempty"`

	OccurredAt time.Time `json:"occurredAt"`
}

// AuditEventIDPrefix and AuditEventIDHexLength describe a generated id.
const (
	AuditEventIDPrefix    = "aud_"
	AuditEventIDHexLength = 20
)

// NewAuditEventID generates a public identifier.
func NewAuditEventID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return AuditEventIDPrefix + encodeHex(raw[:])
}

// ValidAuditEventID reports whether id has the shape of a generated id.
func ValidAuditEventID(id string) bool {
	if len(id) != len(AuditEventIDPrefix)+AuditEventIDHexLength {
		return false
	}
	if id[:len(AuditEventIDPrefix)] != AuditEventIDPrefix {
		return false
	}
	for _, r := range id[len(AuditEventIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// AuditSummary is the aggregate the audit page renders.
type AuditSummary struct {
	Total int `json:"total"`
	// FailedLogins and DeniedActions over the recent window are the two numbers
	// an administrator opens this page for.
	FailedLogins  int `json:"failedLogins"`
	DeniedActions int `json:"deniedActions"`
	// PrivilegedActions counts host-changing actions in the same window.
	PrivilegedActions int `json:"privilegedActions"`
	// WindowHours is how far back the counts above look.
	WindowHours int `json:"windowHours"`

	ByAction  map[AuditAction]int  `json:"byAction"`
	ByOutcome map[AuditOutcome]int `json:"byOutcome"`

	LastEventAt *time.Time `json:"lastEventAt,omitempty"`
}

// ------------------------------------------------------- client addresses --

// NormaliseClientAddr renders a source address for storage and display.
//
// # The port is dropped
//
// A source port is ephemeral, identifies nothing after the connection closes,
// and is the field most likely to make two records of the same client look
// different. Dropping it makes "everything from this address" a query rather
// than a pattern match.
//
// # IPv6 is canonicalised
//
// The same address has many spellings, and an audit log where 2001:db8::1 and
// 2001:0db8:0000:0000:0000:0000:0000:0001 are different rows is a log that
// hides what it should surface.
//
// # Anything unparseable becomes "unknown"
//
// Rather than being stored as given. The value reaches an audit row and a
// browser, and a field that can hold arbitrary text is a field that will
// eventually hold markup.
func NormaliseClientAddr(remoteAddr string) string {
	trimmed := strings.TrimSpace(remoteAddr)
	if trimmed == "" {
		return "unknown"
	}

	// net.SplitHostPort fails on a bare address, which is a legitimate input
	// here -- a trusted-proxy header carries no port.
	host := trimmed
	if split, _, err := net.SplitHostPort(trimmed); err == nil {
		host = split
	}
	host = strings.TrimSpace(host)

	// A zone index (fe80::1%eth0) is local to the machine that produced it and
	// means nothing in a stored record.
	if index := strings.IndexByte(host, '%'); index >= 0 {
		host = host[:index]
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	// An IPv4-mapped IPv6 address is an IPv4 address wearing a hat, and storing
	// both forms would split one client across two rows.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr.String()
}

// ------------------------------------------------------------- requesters --

// Requester is the account that asked for a piece of asynchronous work.
//
// # Why this is not the full audit actor
//
// An audit event records five things about who acted: the user, the username,
// the role, the session, and the address. A Requester records TWO, and the
// omissions are deliberate.
//
//   - No role. A role is what the authorization decision used at the moment of
//     the request; storing it on a record that outlives the request invites a
//     reader to treat it as current, and it is not.
//   - No session. A session identifier is only meaningful while the session
//     exists, and this record outlives it.
//   - No address. The address belongs to the REQUEST, which is already audited
//     with it. Repeating it on the record would be a second copy that can
//     disagree with the first.
//
// What remains is the smallest thing that answers "whose action was this" after
// the request is gone.
type Requester struct {
	UserID   string `json:"userId,omitempty"`
	Username string `json:"username,omitempty"`
}

// Known reports whether an actor was recorded.
//
// False for work requested before HarborMaster recorded requesters, and for
// work started by a path that has no account behind it. The distinction
// matters: "nobody" and "not recorded" are different, and a UI that renders
// them the same is lying about one of them.
func (r Requester) Known() bool { return r.UserID != "" || r.Username != "" }

// Describe renders the requester for an audit reason or a log line.
func (r Requester) Describe() string {
	if r.Username != "" {
		return r.Username
	}
	if r.UserID != "" {
		return r.UserID
	}
	return "an unrecorded account"
}
