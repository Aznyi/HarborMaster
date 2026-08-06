package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Account administration and the security audit log.
//
// Every route in this file is behind PermUserManage or PermAuditRead, declared
// in the route table. No handler here checks a role: by the time one runs, the
// decision has been made.

// userUnavailable writes the disabled response, and reports whether it did.
func (s *Server) userUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.users != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"user administration is not configured")
	return true
}

// userListResponse is the account listing.
type userListResponse struct {
	Items      []publicUserView `json:"items"`
	Pagination Pagination       `json:"pagination"`
}

// handleUsers lists accounts.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if s.userUnavailable(w, r) {
		return
	}

	filter, err := parseUserQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	users, total, err := s.users.List(r.Context(), filter.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "user list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	items := make([]publicUserView, 0, len(users))
	for _, user := range users {
		items = append(items, publicUser(user))
	}

	writeJSON(w, r, s.logger, http.StatusOK, userListResponse{
		Items:      items,
		Pagination: newPagination(filter.Page, filter.PageSize, total),
	})
}

// handleUserDetail returns one account.
func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	if s.userUnavailable(w, r) {
		return
	}
	userID, ok := s.userID(w, r)
	if !ok {
		return
	}

	user, err := s.users.Get(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "user not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "user load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, publicUser(user))
}

// createUserBody is an administrator adding an account.
//
// Note what is absent: no permission list, no expiry, no "is admin" boolean.
// The ROLE is the whole of the authorization decision, and it comes from a
// closed vocabulary.
type createUserBody struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	// Password is optional. Omitting it makes HarborMaster generate one and
	// return it once, which is the safer default: an administrator never has to
	// invent a password for somebody else and never has one they chose.
	Password string `json:"password,omitempty"`
}

// createdUserResponse carries the new account and, when generated, its
// temporary password.
//
// The password appears HERE and nowhere else, ever. It is not stored in
// plaintext, not logged, and not retrievable again.
type createdUserResponse struct {
	User publicUserView `json:"user"`
	// TemporaryPassword is present only when HarborMaster generated one.
	TemporaryPassword string `json:"temporaryPassword,omitempty"`
}

// handleUserCreate adds an account.
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if s.userUnavailable(w, r) {
		return
	}

	var body createUserBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if len(body.Password) > domain.MaxPasswordBytes {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the password is too long")
		return
	}
	if !domain.ValidRole(body.Role) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the role must be one of viewer, operator, or administrator")
		return
	}

	created, err := s.users.Create(r.Context(), s.actorFrom(r), service.CreateUserRequest{
		Username: body.Username,
		Role:     domain.Role(body.Role),
		Password: body.Password,
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	writeJSON(w, r, s.logger, http.StatusCreated, createdUserResponse{
		User:              publicUser(created.User),
		TemporaryPassword: created.TemporaryPassword,
	})
}

// updateUserBody changes an account's role or status.
//
// Both optional, both pointers, so "not supplied" and "supplied" are
// distinguishable. A plain string would make an omitted field indistinguishable
// from an empty one, and an empty role would then read as a demotion.
type updateUserBody struct {
	Role   *string `json:"role,omitempty"`
	Status *string `json:"status,omitempty"`
}

// handleUserUpdate changes a role or a status.
//
// # Self-modification is refused
//
// An administrator cannot change their own role or disable themselves. Not
// because it would break an invariant -- the last-administrator guard already
// covers that -- but because it is almost always a mistake, and the one
// legitimate case (stepping down) is better done by another administrator who
// can see the estate still has one.
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	if s.userUnavailable(w, r) {
		return
	}
	userID, ok := s.userID(w, r)
	if !ok {
		return
	}

	var body updateUserBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if body.Role == nil && body.Status == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"supply a role, a status, or both")
		return
	}

	identity, _ := IdentityFrom(r.Context())
	if identity.User.UserID == userID {
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"you cannot change your own role or status; ask another administrator")
		return
	}

	actor := s.actorFrom(r)
	var (
		user domain.User
		err  error
	)

	if body.Role != nil {
		if !domain.ValidRole(*body.Role) {
			writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
				"the role must be one of viewer, operator, or administrator")
			return
		}
		if user, err = s.users.SetRole(r.Context(), actor, userID, domain.Role(*body.Role)); err != nil {
			s.writeUserError(w, r, err)
			return
		}
	}

	if body.Status != nil {
		if !domain.ValidUserStatus(*body.Status) {
			writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
				"the status must be one of active or disabled")
			return
		}
		if user, err = s.users.SetStatus(r.Context(), actor,
			userID, domain.UserStatus(*body.Status)); err != nil {
			s.writeUserError(w, r, err)
			return
		}
	}

	writeJSON(w, r, s.logger, http.StatusOK, publicUser(user))
}

// passwordResetResponse carries the generated temporary password.
type passwordResetResponse struct {
	User publicUserView `json:"user"`
	// TemporaryPassword appears here once and is never retrievable again.
	TemporaryPassword string `json:"temporaryPassword"`
}

// handleUserPasswordReset issues a temporary credential for another account.
//
// Every session on the account is revoked, because a reset is what an
// administrator does when they believe an account is compromised.
func (s *Server) handleUserPasswordReset(w http.ResponseWriter, r *http.Request) {
	if s.userUnavailable(w, r) {
		return
	}
	userID, ok := s.userID(w, r)
	if !ok {
		return
	}

	password, err := s.users.ResetPassword(r.Context(), s.actorFrom(r), userID)
	if err != nil {
		s.writeUserError(w, r, err)
		return
	}

	user, err := s.users.Get(r.Context(), userID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "user load after reset failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, passwordResetResponse{
		User:              publicUser(user),
		TemporaryPassword: password,
	})
}

// roleCatalogueResponse describes every role and what it may do.
type roleCatalogueResponse struct {
	Items []domain.RoleDescription `json:"items"`
}

// handleRoles returns the role catalogue.
//
// Served from domain.RoleCatalogue, which is the same table the authorization
// middleware consults. The role picker in the UI is therefore built from the
// source of truth rather than from a second list that can drift from it.
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.logger, http.StatusOK, roleCatalogueResponse{
		Items: domain.RoleCatalogue(),
	})
}

// ------------------------------------------------------------------ audit --

// auditUnavailable writes the disabled response, and reports whether it did.
func (s *Server) auditUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.audit != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"the security audit log is not configured")
	return true
}

// auditListResponse is the audit listing.
type auditListResponse struct {
	Items      []domain.AuditEvent `json:"items"`
	Pagination Pagination          `json:"pagination"`
}

// handleAudit lists security audit events, newest first.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.auditUnavailable(w, r) {
		return
	}

	query, err := parseAuditQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	events, total, err := s.audit.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "audit list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, auditListResponse{
		Items:      events,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleAuditSummary returns the audit aggregate.
func (s *Server) handleAuditSummary(w http.ResponseWriter, r *http.Request) {
	if s.auditUnavailable(w, r) {
		return
	}

	summary, err := s.audit.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "audit summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, summary)
}

// ---------------------------------------------------------------- queries --

// userID reads and validates the {id} path segment.
func (s *Server) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidUserID(raw) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the user id is not well formed")
		return "", false
	}
	return raw, true
}

// userQuery is a parsed and validated account listing request.
type userQuery struct {
	Status   domain.UserStatus
	Role     domain.Role
	Page     int
	PageSize int
}

// parseUserQuery reads and validates the listing parameters.
func parseUserQuery(query url.Values) (userQuery, error) {
	var parsed userQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if !domain.ValidUserStatus(raw) {
			return parsed, invalidParam("status", "active or disabled")
		}
		parsed.Status = domain.UserStatus(raw)
	}
	if raw := strings.TrimSpace(query.Get("role")); raw != "" {
		if !domain.ValidRole(raw) {
			return parsed, invalidParam("role", "viewer, operator, or administrator")
		}
		parsed.Role = domain.Role(raw)
	}
	return parsed, nil
}

func (q userQuery) filter() store.UserFilter {
	return store.UserFilter{
		Status: q.Status,
		Role:   q.Role,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// auditQuery is a parsed and validated audit listing request.
type auditQuery struct {
	Actions      []domain.AuditAction
	Outcomes     []domain.AuditOutcome
	ActorUserID  string
	TargetType   domain.AuditTargetType
	SecurityOnly bool

	Page     int
	PageSize int
}

// parseAuditQuery reads and validates the listing parameters.
//
// Every value is checked against a closed vocabulary defined in the domain
// package, or is a bounded integer, or is an identifier validated by shape.
// Nothing a caller sends becomes SQL text.
func parseAuditQuery(query url.Values) (auditQuery, error) {
	var parsed auditQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	actions, err := parseVocabulary(query, "action", domain.ValidAuditAction,
		"a known audit action")
	if err != nil {
		return parsed, err
	}
	for _, value := range actions {
		parsed.Actions = append(parsed.Actions, domain.AuditAction(value))
	}

	outcomes, err := parseVocabulary(query, "outcome", domain.ValidAuditOutcome,
		"one of succeeded, failed, denied")
	if err != nil {
		return parsed, err
	}
	for _, value := range outcomes {
		parsed.Outcomes = append(parsed.Outcomes, domain.AuditOutcome(value))
	}

	if raw := strings.TrimSpace(query.Get("actorUserId")); raw != "" {
		if !domain.ValidUserID(raw) {
			return parsed, invalidParam("actorUserId", "a well-formed user id")
		}
		parsed.ActorUserID = raw
	}
	if raw := strings.TrimSpace(query.Get("targetType")); raw != "" {
		if !domain.ValidAuditTargetType(raw) {
			return parsed, invalidParam("targetType", "a known target type")
		}
		parsed.TargetType = domain.AuditTargetType(raw)
	}
	if raw := strings.TrimSpace(query.Get("securityOnly")); raw != "" {
		switch raw {
		case "true":
			parsed.SecurityOnly = true
		case "false":
			parsed.SecurityOnly = false
		default:
			return parsed, invalidParam("securityOnly", "true or false")
		}
	}

	return parsed, nil
}

func (q auditQuery) filter() store.AuditFilter {
	return store.AuditFilter{
		Actions:      q.Actions,
		Outcomes:     q.Outcomes,
		ActorUserID:  q.ActorUserID,
		TargetType:   q.TargetType,
		SecurityOnly: q.SecurityOnly,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// writeUserError maps an administration failure onto a status code.
func (s *Server) writeUserError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "user not found")

	case errors.Is(err, store.ErrLastAdministrator):
		// A conflict rather than a bad request: the request was well formed and
		// the estate's state is what refuses it. The message says which, because
		// an administrator needs to know to promote somebody first.
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this is the last active administrator; promote another account first")

	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"that username is already in use")

	default:
		s.writeAuthError(w, r, err)
	}
}
