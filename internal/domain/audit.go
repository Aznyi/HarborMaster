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
	case AuditAcquisitionCompleted, AuditExecutionCompleted:
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
	AuditTargetInventory   AuditTargetType = "inventory"
	AuditTargetSystem      AuditTargetType = "system"
)

// AuditTargetTypes lists every target type.
var AuditTargetTypes = []AuditTargetType{
	AuditTargetUser, AuditTargetSession, AuditTargetContainer,
	AuditTargetSnapshot, AuditTargetDrift, AuditTargetPolicy,
	AuditTargetViolation, AuditTargetPlan, AuditTargetAcquisition,
	AuditTargetExecution, AuditTargetInventory, AuditTargetSystem,
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
