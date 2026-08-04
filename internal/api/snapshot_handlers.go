package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// SnapshotReader is the snapshot query capability the API depends on.
//
// Narrow interfaces, not the concrete service, so handlers can be tested with
// simple doubles and no database.
type SnapshotReader interface {
	List(ctx context.Context, filter store.SnapshotFilter) ([]domain.Snapshot, int, error)
	Get(ctx context.Context, id int64) (domain.Snapshot, error)
	Environment(ctx context.Context, snapshotID int64) ([]domain.SnapshotEnvEntry, error)
	Mounts(ctx context.Context, snapshotID int64) ([]domain.SnapshotMountRow, error)
	Networks(ctx context.Context, snapshotID int64) ([]domain.SnapshotNetworkRow, error)
}

// SnapshotCapturer records a new snapshot.
//
// Capture writes HarborMaster's own database. It has no ability to change a
// container, and there is no corresponding "apply" or "restore" capability
// anywhere in this package.
type SnapshotCapturer interface {
	Enabled() bool
	Capture(ctx context.Context, req service.CaptureRequest) (domain.Snapshot, error)
	CaptureStartedAt(containerID string) (time.Time, bool)
}

// SnapshotDiffer compares two configurations.
type SnapshotDiffer interface {
	Diff(ctx context.Context, from, to service.DiffInput, opts service.DiffOptions) (domain.SnapshotDiff, error)
}

// SnapshotReadinessEvaluator validates whether a snapshot could be restored.
type SnapshotReadinessEvaluator interface {
	Evaluate(
		ctx context.Context,
		snapshot domain.Snapshot,
		env []domain.SnapshotEnvEntry,
		mounts []domain.SnapshotMountRow,
		networks []domain.SnapshotNetworkRow,
	) (domain.ReadinessReport, error)
}

// SnapshotDetailResponse is one snapshot with its derived sections.
type SnapshotDetailResponse struct {
	domain.Snapshot
	Environment []domain.SnapshotEnvEntry   `json:"environment"`
	Mounts      []domain.SnapshotMountRow   `json:"mounts"`
	Networks    []domain.SnapshotNetworkRow `json:"networks"`
}

// createSnapshotRequest is the POST /snapshots body.
//
// Decoded with DisallowUnknownFields, so an unrecognised key is a 400 rather
// than a silently ignored option.
type createSnapshotRequest struct {
	ContainerID string `json:"containerId"`
	Reason      string `json:"reason"`
}

// handleSnapshots serves the snapshot list.
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return
	}

	filter, err := parseSnapshotFilter(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	snapshots, total, err := s.snapshots.List(r.Context(), filter)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// The list omits the canonical document: a page of fifty snapshots would
	// otherwise carry fifty full configurations, and the list view shows none
	// of it.
	for i := range snapshots {
		snapshots[i].SpecJSON = nil
	}

	pageSize := filter.Page.Limit
	page := 1
	if pageSize > 0 {
		page = filter.Page.Offset/pageSize + 1
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.Snapshot]{
		Items:      snapshots,
		Pagination: newPagination(page, pageSize, total),
	})
}

// handleSnapshotDetail serves one snapshot with its derived sections.
func (s *Server) handleSnapshotDetail(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return
	}

	snapshot, ok := s.loadSnapshot(w, r)
	if !ok {
		return
	}

	env, mounts, networks, ok := s.loadSnapshotSections(w, r, snapshot.ID)
	if !ok {
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, SnapshotDetailResponse{
		Snapshot:    snapshot,
		Environment: env,
		Mounts:      mounts,
		Networks:    networks,
	})
}

// handleSnapshotCreate captures a snapshot.
//
// This writes HarborMaster's own database and reads HarborMaster's own
// inventory. It cannot create, change, or remove a container, and no code path
// from here reaches the Docker runtime.
func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return
	}
	if !s.capture.Enabled() {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are disabled by configuration")
		return
	}

	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	var body createSnapshotRequest
	if err := decodeJSONBody(w, r, s.maxRequestBytes(), &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	if err := s.validateCreateRequest(body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	snapshot, err := s.capture.Capture(r.Context(), service.CaptureRequest{
		ContainerID: body.ContainerID,
		Trigger:     domain.SnapshotTriggerAPI,
		Reason:      body.Reason,
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "container not found")
		return
	case errors.Is(err, service.ErrCaptureInProgress):
		s.writeCaptureConflict(w, r, body.ContainerID)
		return
	case errors.Is(err, service.ErrSnapshotsDisabled):
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are disabled by configuration")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "snapshot capture failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// 200 rather than 201 when the configuration was unchanged: nothing was
	// created, and a client that treats 201 as "new checkpoint recorded" would
	// otherwise be misled.
	status := http.StatusCreated
	if snapshot.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, r, s.logger, status, snapshot)
}

// validateCreateRequest checks the decoded body.
func (s *Server) validateCreateRequest(body createSnapshotRequest) error {
	if body.ContainerID == "" {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "containerId is required",
		}
	}
	if err := validateText("containerId", body.ContainerID, maxContainerRefLength); err != nil {
		return err
	}

	maxReason := s.snapshotCfg.MaxReasonBytes
	if maxReason <= 0 {
		maxReason = 500
	}
	return validateText("reason", body.Reason, maxReason)
}

// writeCaptureConflict reports an in-flight capture as 409.
func (s *Server) writeCaptureConflict(w http.ResponseWriter, r *http.Request, containerID string) {
	body := struct {
		Error     ErrorBody `json:"error"`
		RequestID string    `json:"requestId,omitempty"`
		Active    struct {
			InProgress bool      `json:"inProgress"`
			StartedAt  time.Time `json:"startedAt"`
		} `json:"active"`
	}{
		Error: ErrorBody{
			Code:    CodeConflict,
			Message: "a snapshot capture is already in progress for this container",
		},
		RequestID: RequestIDFrom(r.Context()),
	}
	body.Active.InProgress = true
	if startedAt, running := s.capture.CaptureStartedAt(containerID); running {
		body.Active.StartedAt = startedAt
	}

	writeJSON(w, r, s.logger, http.StatusConflict, body)
}

// handleSnapshotDiff compares a snapshot against another or against current.
func (s *Server) handleSnapshotDiff(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil || s.diffs == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return
	}

	from, ok := s.loadSnapshot(w, r)
	if !ok {
		return
	}

	query, err := parseDiffQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	fromInput, ok := s.diffInput(w, r, from)
	if !ok {
		return
	}

	var (
		toInput        service.DiffInput
		againstCurrent = query.AgainstCurrent
	)

	if query.AgainstCurrent {
		// "Current" means the container's live configuration, captured in
		// memory and never written: comparing against the present must not
		// create a snapshot as a side effect.
		current, ok := s.currentDiffInput(w, r, from.ContainerID)
		if !ok {
			return
		}
		toInput = current
	} else {
		to, err := s.snapshots.Get(r.Context(), query.AgainstSnapshotID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "comparison snapshot not found")
			return
		}
		if err != nil {
			s.logger.ErrorContext(r.Context(), "diff target load failed", slog.String("error", err.Error()))
			writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
			return
		}
		if toInput, ok = s.diffInput(w, r, to); !ok {
			return
		}
	}

	diff, err := s.diffs.Diff(r.Context(), fromInput, toInput, service.DiffOptions{
		Groups:           query.Groups,
		IncludeUnchanged: query.IncludeUnchanged,
		AgainstCurrent:   againstCurrent,
	})
	switch {
	case errors.Is(err, service.ErrDiffBusy):
		// Refused rather than queued. Retry-After gives the client something
		// actionable instead of an immediate retry storm.
		w.Header().Set("Retry-After", "2")
		writeError(w, r, s.logger, http.StatusTooManyRequests, CodeConflict,
			"too many comparisons in progress; retry shortly")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "snapshot diff failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, diff)
}

// handleSnapshotReadiness evaluates whether a snapshot could be restored.
//
// The evaluation is informational. HarborMaster cannot restore a container, and
// nothing this endpoint does brings it closer to being able to.
func (s *Server) handleSnapshotReadiness(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil || s.readiness == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return
	}

	snapshot, ok := s.loadSnapshot(w, r)
	if !ok {
		return
	}

	env, mounts, networks, ok := s.loadSnapshotSections(w, r, snapshot.ID)
	if !ok {
		return
	}

	report, err := s.readiness.Evaluate(r.Context(), snapshot, env, mounts, networks)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "readiness evaluation failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, report)
}

// loadSnapshot resolves the {id} path value, writing the error response itself.
func (s *Server) loadSnapshot(w http.ResponseWriter, r *http.Request) (domain.Snapshot, bool) {
	id, err := parseSnapshotID(r.PathValue("id"))
	if err != nil {
		s.writeQueryError(w, r, err)
		return domain.Snapshot{}, false
	}

	snapshot, err := s.snapshots.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "snapshot not found")
		return domain.Snapshot{}, false
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return domain.Snapshot{}, false
	}
	return snapshot, true
}

// loadSnapshotSections loads the derived child rows.
func (s *Server) loadSnapshotSections(w http.ResponseWriter, r *http.Request, id int64) (
	[]domain.SnapshotEnvEntry, []domain.SnapshotMountRow, []domain.SnapshotNetworkRow, bool,
) {
	env, err := s.snapshots.Environment(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot environment load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return nil, nil, nil, false
	}

	mounts, err := s.snapshots.Mounts(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot mounts load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return nil, nil, nil, false
	}

	networks, err := s.snapshots.Networks(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot networks load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return nil, nil, nil, false
	}

	return env, mounts, networks, true
}

// diffInput assembles one side of a comparison from a stored snapshot.
func (s *Server) diffInput(w http.ResponseWriter, r *http.Request, snapshot domain.Snapshot) (service.DiffInput, bool) {
	var spec domain.SnapshotSpec
	if len(snapshot.SpecJSON) > 0 {
		if err := json.Unmarshal(snapshot.SpecJSON, &spec); err != nil {
			s.logger.ErrorContext(r.Context(), "snapshot document could not be decoded",
				slog.Int64("snapshotId", snapshot.ID), slog.String("error", err.Error()))
			writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
			return service.DiffInput{}, false
		}
	}

	env, err := s.snapshots.Environment(r.Context(), snapshot.ID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "snapshot environment load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return service.DiffInput{}, false
	}

	return service.DiffInput{SnapshotID: snapshot.ID, Spec: spec, Env: env}, true
}

// currentDiffInput builds the live configuration side of a comparison.
//
// Captured in memory and discarded. Comparing against "current" must not write
// a snapshot as a side effect: a GET that creates a durable record would make
// every diff request a database write, and an unauthenticated caller could fill
// the table by polling.
func (s *Server) currentDiffInput(w http.ResponseWriter, r *http.Request, containerID string) (service.DiffInput, bool) {
	if s.containers == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"container inventory is not configured")
		return service.DiffInput{}, false
	}

	detail, err := s.containers.Get(r.Context(), containerID)
	if errors.Is(err, store.ErrNotFound) || detail == nil {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"the container this snapshot describes is no longer in the inventory")
		return service.DiffInput{}, false
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "current container load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return service.DiffInput{}, false
	}

	if s.snapshotSpecBuilder == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"snapshots are not configured")
		return service.DiffInput{}, false
	}

	spec := s.snapshotSpecBuilder(*detail)
	return service.DiffInput{SnapshotID: 0, Spec: spec}, true
}

// maxRequestBytes reports the configured body limit.
func (s *Server) maxRequestBytes() int64 {
	if s.cfg.MaxRequestBytes > 0 {
		return s.cfg.MaxRequestBytes
	}
	return 1 << 20
}
