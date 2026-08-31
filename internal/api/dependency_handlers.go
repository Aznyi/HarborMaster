package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The workload dependency endpoints.
//
// # What this file may not do, and what stops it
//
// Nothing here reaches Docker. This file does not import internal/docker, does
// not name a mutation capability, and does not call the acquisition, execution,
// or rollback services. Three architecture tests hold each of those by AST, and
// they are non-vacuous because this file exists.
//
// The reason it matters: a dependency relationship decides whether a container
// may be recreated, and in one case that a container MUST be. That is close
// enough to a mutation to be worth proving it is not one. Everything here reads
// a projection or writes one small table.
//
// # The create request carries two names
//
// No image, no digest, no registry, no namespace, no container id, no Docker
// option. Unknown fields are rejected outright, so a client that sends one gets
// a 400 rather than having it silently ignored -- which is how somebody comes to
// believe an option took effect.

// DependencyService is the surface the dependency endpoints depend on.
//
// # Read what is NOT here
//
// No acquisition, no execution, no rollback, no plan generation, and nothing
// that names a container id, an image, or a digest. Three reads and two writes
// against HarborMaster's own records, and the create takes two container NAMES.
//
// That is the interface-level statement of "the API cannot become a second
// mutation path": there is no method here to call, however a handler is written.
type DependencyService interface {
	List(ctx context.Context) (service.DependencyListing, error)
	ForContainer(ctx context.Context, name string) (service.ContainerDependencies, error)
	Graph(ctx context.Context) (service.DependencyGraphProjection, error)
	// AttentionFacts is the whole-estate dependency picture a container LIST
	// needs, gathered once. Keyed on container name.
	//
	// A read for DISPLAY. It is consulted while rendering a page, so it is the
	// one dependency call that must never cost a query per row.
	AttentionFacts(ctx context.Context) (map[string]service.DependencyFacts, error)
	// RecentOperations lists coordinated provider updates. A READ: it creates
	// no plan, submits nothing, and reaches no Docker socket.
	RecentOperations(ctx context.Context, limit int) ([]service.DependencyOperationSummary, error)

	CreateOperatorDependency(ctx context.Context, dependent, dependency string,
		requestedBy domain.Requester) (domain.WorkloadDependency, error)
	DeleteOperatorDependency(ctx context.Context, dependencyID string) (domain.WorkloadDependency, error)
}

// dependencyCreateRequest is the whole of what a caller may say.
//
// Two container NAMES. Deliberately not a struct that could grow: an
// architecture test walks its fields and fails on any name resembling an image,
// a digest, a container id, or a Docker option.
type dependencyCreateRequest struct {
	// Dependent needs Dependency. Execution order is the reverse of the arrow.
	Dependent  string `json:"dependent"`
	Dependency string `json:"dependency"`
}

// dependencyResponse is one relationship as the API renders it.
type dependencyResponse struct {
	// DependencyID is present for OPERATOR relationships only. A discovered one
	// is derived from the inventory on every read and has no stored identity --
	// which is also why it cannot be deleted.
	DependencyID string                  `json:"dependencyId,omitempty"`
	Dependent    string                  `json:"dependent"`
	Dependency   string                  `json:"dependency"`
	Source       domain.DependencySource `json:"source"`
	// Kind and Origin are the operator-facing renderings, so a UI does not have
	// to carry its own copy of the vocabulary.
	Kind   string `json:"kind"`
	Origin string `json:"origin"`
	// Hard reports that the runtime itself requires the relationship.
	Hard bool `json:"hard"`
	// Why is HarborMaster's own sentence explaining the relationship.
	Why string `json:"why"`
	// Deletable reports whether this relationship can be removed. False for
	// every discovered one.
	Deletable bool                      `json:"deletable"`
	Evidence  domain.DependencyEvidence `json:"evidence,omitzero"`
	CreatedBy domain.Requester          `json:"createdBy,omitzero"`
}

// renderDependency projects one relationship for the API.
func renderDependency(edge domain.WorkloadDependency) dependencyResponse {
	return dependencyResponse{
		DependencyID: edge.DependencyID,
		Dependent:    edge.Dependent,
		Dependency:   edge.Dependency,
		Source:       edge.Source,
		Kind:         edge.Source.Describe(),
		Origin:       edge.Source.Origin(),
		Hard:         edge.Hard(),
		Why:          edge.Source.Explain(),
		// Only an operator relationship has a stored row to remove.
		Deletable: edge.Source == domain.DependencyOperator && edge.DependencyID != "",
		Evidence:  edge.Evidence,
		CreatedBy: edge.CreatedBy,
	}
}

func renderDependencies(edges []domain.WorkloadDependency) []dependencyResponse {
	out := make([]dependencyResponse, 0, len(edges))
	for _, edge := range edges {
		out = append(out, renderDependency(edge))
	}
	return out
}

// dependencyUnavailable reports 503 when the subsystem is not wired.
func (s *Server) dependencyUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.dependencies == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"dependency tracking is not available in this deployment")
		return true
	}
	return false
}

// dependencyFacts returns the whole-estate dependency picture for a page render.
//
// # Why a failure here is silence rather than a refusal
//
// This is a DISPLAY read. When the graph cannot be built the container list
// renders exactly as it did before dependencies existed: every row keeps its
// other evidence and none of them claims anything about a dependency, because
// `DependencyKnown` stays false and the assessment asserts nothing from it.
//
// That is not a weakened safety check. The safety decisions are made in the
// automation gate and the execution preflight, both of which fail CLOSED on the
// same condition and neither of which consults this. Refusing to render the
// inventory because a graph could not be built would take away the page an
// operator needs to diagnose exactly that.
func (s *Server) dependencyFacts(ctx context.Context) map[string]service.DependencyFacts {
	if s.dependencies == nil {
		return nil
	}
	facts, err := s.dependencies.AttentionFacts(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "the dependency picture could not be read for a listing",
			slog.String("error", err.Error()))
		return nil
	}
	return facts
}

// dependencyFailure renders a service error.
//
// A graph that could not be built is 503 rather than 500: it is a statement
// about the estate being un-orderable right now -- too many containers, an
// unreadable inventory -- not about HarborMaster being broken. The underlying
// error is never returned, only logged.
func (s *Server) dependencyFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, service.ErrDependencyGraphUnavailable) {
		s.logger.WarnContext(r.Context(), "the dependency graph could not be built",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"the dependency graph could not be built for this host")
		return
	}
	s.logger.ErrorContext(r.Context(), "dependency request failed",
		slog.String("error", err.Error()))
	writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
}

// ------------------------------------------------------------------ reads --

// handleDependencies lists every relationship in force.
func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}

	listing, err := s.dependencies.List(r.Context())
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	// The counts a dashboard needs, on the response it was already going to
	// make. One read for the whole picture rather than three, and derived from
	// the same facts the container rows use -- so the dashboard number and the
	// row badges cannot disagree about how many loops there are.
	summary := service.Summarise(s.dependencyFacts(r.Context()))

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"items":   renderDependencies(listing.Edges),
		"total":   len(listing.Edges),
		"summary": summary,
		// Reported alongside, never instead of. A container whose namespace
		// share could not be resolved is dependency-blocked, and omitting that
		// from a listing would make the estate look tidier than it is.
		"problems":   listing.Problems,
		"unresolved": listing.Unresolved,
	})
}

// handleDependenciesForContainer returns one container's two directions.
//
// Takes the container's ID in the path, matching every other per-container
// route, and resolves it to the stable NAME the relationships are keyed on.
func (s *Server) handleDependenciesForContainer(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}
	if s.containers == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"the container inventory is not available in this deployment")
		return
	}

	containerID := strings.TrimSpace(r.PathValue("id"))
	// Resolved through the inventory rather than trusted. The path value is
	// caller input; the NAME the lookup returns is a record HarborMaster wrote.
	//
	// GetPresent, not Get. This endpoint answers a CURRENT question -- the
	// state, the stage, the outstanding reattachments -- and it resolves the id
	// to a NAME before reading. With Get, a departed container's id resolved to
	// its name and the response described whichever container holds that name
	// NOW: one instance's operational state returned under another instance's
	// id. A departed container has no current dependencies, and saying so is
	// the only truthful answer available.
	detail, err := s.containers.GetPresent(r.Context(), containerID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && detail == nil) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "container not found")
		return
	}
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	result, err := s.dependencies.ForContainer(r.Context(), detail.Overview.Name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "container not found")
		return
	}
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"container":          result.Container,
		"dependsOn":          renderDependencies(result.DependsOn),
		"dependedOnBy":       renderDependencies(result.DependedOnBy),
		"stage":              result.Stage,
		"state":              result.State,
		"detail":             result.Detail,
		"problems":           result.Problems,
		"outstandingRebinds": result.OutstandingRebinds,
	})
}

// handleDependencyGraph returns the deterministic update order.
//
// A PROJECTION over work that is already valid. It creates no plan, approves
// nothing, and reaches no Docker socket.
func (s *Server) handleDependencyGraph(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}

	projection, err := s.dependencies.Graph(r.Context())
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"stages":            projection.Stages,
		"cycles":            projection.Cycles,
		"cycleDescriptions": projection.CycleDescriptions,
		"unresolved":        projection.Unresolved,
		"blocked":           projection.Blocked,
		"edges":             renderDependencies(projection.Edges),
	})
}

// handleDependencyOperations lists coordinated provider updates.
//
// # Why this is a read and can only be a read
//
// It reports what HarborMaster DID: which provider was replaced, which
// dependents had to be reattached, and where each of them got to. There is no
// query parameter that selects a target, no body, and no method other than GET
// on this path -- and the service behind it holds no Docker capability, so
// there is nothing for a handler to call even if one were written.
//
// # Why a concluded operation is still listed
//
// The state worth reading is frequently a CONCLUDED one: a provider that
// succeeded with one rebind that did not is recorded as failed, and it is
// exactly the situation an operator has to settle. HarborMaster does not roll a
// dependency group backward, so the successful half stays on the host and the
// listing says so rather than tidying it away.
func (s *Server) handleDependencyOperations(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}

	summaries, err := s.dependencies.RecentOperations(r.Context(), 0)
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"items": summaries,
		"total": len(summaries),
		// Stated rather than implied. A caller that read `items` alone could
		// not tell a quiet host from a truncated listing.
		"limit": maxReportedDependencyOperations,
	})
}

// maxReportedDependencyOperations mirrors the store's own cap, so the response
// can say what it is rather than leaving a reader to infer it.
const maxReportedDependencyOperations = 50

// ----------------------------------------------------------------- writes --

// maxDependencyNameBytes bounds a submitted container name.
//
// The same bound a name HarborMaster will create is held to, so a name this
// accepts is one the rest of the system can act on.
const maxDependencyNameBytes = domain.MaxContainerNameBytes

// handleDependencyCreate records an operator-defined ordering.
func (s *Server) handleDependencyCreate(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}
	if err := s.guardPolicyWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	var body dependencyCreateRequest
	// Strict: unknown fields are refused, the body is bounded, exactly one JSON
	// object is accepted, and invalid UTF-8 is rejected rather than replaced.
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// Text validation before anything else looks at the values: bounded length,
	// valid UTF-8, no control characters. The last is what stops a name forging
	// a line in the audit log or the application log.
	for field, value := range map[string]string{
		"dependent":  body.Dependent,
		"dependency": body.Dependency,
	} {
		if err := validateText(field, value, maxDependencyNameBytes); err != nil {
			s.writeGuardFailure(w, r, err)
			return
		}
	}

	created, err := s.dependencies.CreateOperatorDependency(
		r.Context(), body.Dependent, body.Dependency, s.requesterFrom(r))

	var refused service.ErrDependencyRefused
	if errors.As(err, &refused) {
		// A refused write is audited. Somebody enumerating what they may relate
		// looks exactly like this, and so does an integration in a retry loop.
		s.auditRefusal(r, domain.AuditDependencyCreated, domain.AuditTargetDependency,
			"", string(refused.Refusal))
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict, refused.Refusal.Explain())
		return
	}
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	// The audit reason is HarborMaster's own sentence, built from two names that
	// have already passed ValidContainerName -- whose allowlist excludes every
	// character that could forge a log line.
	s.auditWrite(r, domain.AuditDependencyCreated, domain.AuditTargetDependency,
		created.DependencyID, created.Dependent,
		domain.DescribeOperatorDependency(created.Dependent, created.Dependency))

	writeJSON(w, r, s.logger, http.StatusCreated, renderDependency(created))
}

// handleDependencyDelete removes an operator-defined ordering.
//
// # Why this is administrator-only
//
// It removes a SAFETY CONSTRAINT. Creating a relationship can only make
// HarborMaster wait or refuse; deleting one lets an update proceed that was
// being held back.
//
// It cannot reach a discovered relationship. Those are derived from the
// inventory on every read and have no stored row, so there is no id here that
// could name one.
func (s *Server) handleDependencyDelete(w http.ResponseWriter, r *http.Request) {
	if s.dependencyUnavailable(w, r) {
		return
	}
	if err := s.guardPolicyWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	dependencyID := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidDependencyID(dependencyID) {
		// The same 404 an unknown id gets. A malformed id must not be
		// distinguishable from an absent one, or the endpoint becomes an oracle
		// for which ids exist.
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "dependency not found")
		return
	}

	removed, err := s.dependencies.DeleteOperatorDependency(r.Context(), dependencyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "dependency not found")
		return
	}
	if err != nil {
		s.dependencyFailure(w, r, err)
		return
	}

	s.auditWrite(r, domain.AuditDependencyDeleted, domain.AuditTargetDependency,
		removed.DependencyID, removed.Dependent,
		"the ordering between "+removed.Dependent+" and "+removed.Dependency+
			" is no longer enforced")

	w.WriteHeader(http.StatusNoContent)
}
