package api

import (
	"context"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The access model.
//
// # Default deny, expressed as a type
//
// Every route in HarborMaster declares an access policy. The zero value of
// routeAccess is INVALID -- not "public", not "authenticated", invalid -- so a
// route registered without one is refused at runtime and fails
// TestEveryRouteDeclaresAnAccessPolicy at build time.
//
// That is the whole reason this is a type rather than a boolean or a string on
// the side. "I forgot" and "I meant public" have to look different, and with a
// zero value that means public they do not.
//
// # Authorization happens in one place
//
// Handlers never ask about roles. They receive an already-authorized request
// and an identity, and the identity is there for AUDIT ATTRIBUTION rather than
// for a second decision. An architecture test fails the build if any handler
// file names a role constant.

// accessKind is how a route decides who may reach it.
type accessKind string

const (
	// accessInvalid is the zero value. A route carrying it is a bug, and both
	// the test and the runtime treat it as deny.
	accessInvalid accessKind = ""
	// accessPublic needs no session.
	//
	// Exactly four routes hold it, and each is listed with its reason in
	// routeTable. Adding a fifth is a deliberate act visible in a diff.
	accessPublic accessKind = "public"
	// accessBootstrap is reachable ONLY while the installation is unclaimed.
	// Once an administrator exists it behaves as if it did not exist.
	accessBootstrap accessKind = "bootstrap"
	// accessAuthenticated needs a valid session but no particular permission.
	accessAuthenticated accessKind = "authenticated"
	// accessPermission needs a valid session holding a named permission.
	accessPermission accessKind = "permission"
)

// routeAccess declares who may reach a route.
type routeAccess struct {
	kind       accessKind
	permission domain.Permission
	// allowPasswordChange lets a route through for an account that must change
	// its password. Only the password-change and session routes set it: an
	// account with a temporary credential is authenticated but must not be able
	// to do anything else first.
	allowPasswordChange bool
	// skipCSRF exempts a state-changing route from the CSRF header.
	//
	// Only login and bootstrap, which have no session to derive a token from.
	// Both are still protected by SameSite, the Origin and Fetch Metadata
	// checks, and the JSON content-type requirement -- see checkCSRF.
	skipCSRF bool
	// policyRate charges this route to the POLICY write budget instead of the
	// general one.
	//
	// Policy administration is a burst activity -- an administrator writing a
	// rule set edits several in a row -- and each write triggers a compliance
	// sweep rather than a Docker call. It gets its own, more permissive bucket
	// for that reason; see guardPolicyWrite. A route must be charged to ONE
	// bucket, or the more permissive budget is capped by the stricter one and
	// exists only on paper.
	policyRate bool
	// methodNotAllowed marks the bare entry that answers 405.
	//
	// It still needs a session -- otherwise a 405 would confirm to an anonymous
	// scanner that the path exists -- but it must NOT run the write
	// protections. A DELETE against a read-only path is not a write; charging
	// it to the write rate limiter would let a scanner exhaust an operator's
	// budget with requests that were always going to be refused, and answering
	// 429 instead of 405 would tell them nothing true.
	methodNotAllowed bool
}

// valid reports whether the policy was actually declared.
//
// accessInvalid is named explicitly rather than falling into the default,
// because the zero value is the case this function exists for: a route whose
// policy was never set must be refused, and the reader should see that decision
// written down rather than infer it from an absence.
func (a routeAccess) valid() bool {
	switch a.kind {
	case accessInvalid:
		return false
	case accessPublic, accessBootstrap, accessAuthenticated:
		return true
	case accessPermission:
		return a.permission != "" && domain.ValidPermission(string(a.permission))
	default:
		return false
	}
}

// Policy constructors.
//
// Named functions rather than struct literals at 60 call sites, so the table
// reads as a policy document and a typo is a compile error.

// public marks a route reachable without a session.
func public() routeAccess {
	return routeAccess{kind: accessPublic, skipCSRF: true, allowPasswordChange: true}
}

// bootstrapOnly marks a route reachable only on an unclaimed installation.
func bootstrapOnly() routeAccess {
	return routeAccess{kind: accessBootstrap, skipCSRF: true, allowPasswordChange: true}
}

// authenticated marks a route needing any valid session.
func authenticated() routeAccess {
	return routeAccess{kind: accessAuthenticated}
}

// duringPasswordChange marks a route an account with a temporary credential may
// still reach.
func duringPasswordChange() routeAccess {
	return routeAccess{kind: accessAuthenticated, allowPasswordChange: true}
}

// requires marks a route needing a named permission.
func requires(permission domain.Permission) routeAccess {
	return routeAccess{kind: accessPermission, permission: permission}
}

// noCSRF exempts a policy from the CSRF header requirement.
//
// Used on login only. Spelled as a modifier so the exemption appears in the
// route table beside the route that has it, rather than being buried in a
// condition somewhere in the middleware.
func (a routeAccess) noCSRF() routeAccess {
	a.skipCSRF = true
	return a
}

// policyBudget charges a route to the policy write limiter.
//
// Spelled as a modifier on the route so the bucket a write is charged to is
// visible beside the route, rather than being a fact you learn by reading the
// handler.
func (a routeAccess) policyBudget() routeAccess {
	a.policyRate = true
	return a
}

// ------------------------------------------------------------- identity --

// identityKey is the context key carrying the authenticated identity.
type identityContextKey struct{}

// withIdentity attaches an identity to a request context.
func withIdentity(ctx context.Context, identity service.Authenticated) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFrom returns the authenticated identity, if any.
//
// A handler on an authenticated route can rely on this being populated: the
// middleware refused the request otherwise. A handler on a public route must
// tolerate the zero value.
func IdentityFrom(ctx context.Context) (service.Authenticated, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(service.Authenticated)
	return identity, ok
}

// actorFrom builds the audit actor for a request.
//
// Every write path calls this rather than assembling the five fields itself,
// so an audit row cannot be written with the request id of one request and the
// identity of another.
func (s *Server) actorFrom(r *http.Request) service.Actor {
	actor := service.Actor{
		RequestID:  RequestIDFrom(r.Context()),
		ClientAddr: s.clientAddr(r),
	}

	identity, ok := IdentityFrom(r.Context())
	if !ok {
		return actor
	}

	actor.UserID = identity.User.UserID
	actor.Username = identity.User.Username
	actor.Role = identity.User.Role
	actor.SessionID = identity.Session.SessionID
	return actor
}

// requesterFrom projects the caller onto the actor stored with async work.
//
// Two fields only -- see domain.Requester for why the role, session, and
// address are deliberately left behind. The zero value is correct for a caller
// with no identity: the record then reads "not recorded" rather than being
// attributed to nobody in particular.
func (s *Server) requesterFrom(r *http.Request) domain.Requester {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		return domain.Requester{}
	}
	return domain.Requester{
		UserID:   identity.User.UserID,
		Username: identity.User.Username,
	}
}

// auditWrite records who performed an operational write.
//
// # Why every write handler calls this
//
// Before Phase 9.5 every feature recorded WHAT happened -- a snapshot row, an
// acquisition record, an execution record -- and none recorded WHO. This is the
// one line that closes that gap, and it is a call at each write's success point
// rather than a wrapper, because only the handler knows what the write acted
// on.
//
// TestEveryWriteRouteIsAudited walks the route table, finds every
// state-changing entry, and fails if its handler does not call this. That is
// what makes "every write is attributed" a checked property rather than a
// habit.
func (s *Server) auditWrite(
	r *http.Request,
	action domain.AuditAction,
	targetType domain.AuditTargetType,
	targetID, targetName, reason string,
) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAction(r.Context(), s.actorFrom(r), action,
		domain.AuditSucceeded, targetType, targetID, targetName, reason)
}

// deduplicatedReason renders whether a snapshot capture created a new row.
//
// The audit log records the OUTCOME an operator cares about: a capture that
// deduplicated changed nothing, and reading a row that says "created" for it
// would be misleading.
func deduplicatedReason(deduplicated bool) string {
	if deduplicated {
		return "configuration unchanged; the existing snapshot was returned"
	}
	return "a new snapshot was recorded"
}
