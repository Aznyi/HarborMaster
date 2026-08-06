package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// APIPrefix is the versioned base path for every JSON endpoint.
const APIPrefix = "/api/v1"

// Server wires the HTTP routes to the application layer.
type Server struct {
	health     HealthChecker
	inventory  InventoryReader
	containers ContainerReader
	warnings   WarningReader
	images     ImageReader
	networks   NetworkReader
	volumes    VolumeReader

	dockerEvents DockerEventReader
	eventEngine  EventEngineReader

	snapshots SnapshotReader
	capture   SnapshotCapturer
	diffs     SnapshotDiffer
	readiness SnapshotReadinessEvaluator

	// drift answers the drift endpoints. Nil in a deployment with drift
	// detection disabled, which yields a 503 rather than a broken route.
	drift DriftReader
	// driftCfg carries the note length bound for the PATCH endpoint.
	driftCfg config.Drift

	// policies answers the policy endpoints, and policyEngine schedules the
	// manual pass. Both nil in a deployment with the policy engine disabled,
	// which yields a 503 rather than a broken route.
	policies     PolicyStore
	policyEngine PolicyEvaluator
	// policyCfg carries the definition and note bounds the API validates
	// against, so one configured value governs both what the API accepts and
	// what the engine can be asked to evaluate.
	policyCfg config.Policy

	// imageIntel answers the image update endpoints, and imageCollector
	// schedules a metadata collection pass. Both nil in a deployment with image
	// intelligence disabled, which yields a 503 rather than a broken route.
	imageIntel     ImageIntelReader
	imageCollector ImageIntelCollector

	// plans answers the change plan endpoints, and planner schedules a
	// generation pass. Both nil in a deployment with planning disabled, which
	// yields a 503 rather than a broken route.
	plans   PlanReader
	planner PlanGenerator
	// now is injectable so a status change's timestamp is deterministic in
	// tests.
	now func() time.Time
	// snapshotSpecBuilder renders a container's CURRENT configuration as a
	// canonical document, in memory, for a diff against the present. It never
	// persists anything: a GET must not create a durable record.
	snapshotSpecBuilder func(domain.ContainerDetail) domain.SnapshotSpec

	logger *slog.Logger
	cfg    config.Server
	// snapshotCfg carries the write-endpoint and diff limits.
	snapshotCfg config.Snapshots
	// writeLimiter bounds the snapshot and refresh POST endpoints. Per process,
	// not per client: there is no authentication and therefore no trustworthy
	// client identity.
	writeLimiter *rateLimiter
	// policyLimiter bounds the policy write endpoints, on its own more
	// permissive budget. See guardPolicyWrite for why the two differ.
	policyLimiter *rateLimiter

	assets  fs.FS
	handler http.Handler
}

// Options configures a Server.
//
// Every dependency is an interface and every one is optional except Health: a
// nil reader yields a 503 from the routes that need it rather than a panic, so
// a partially configured server still serves what it can.
type Options struct {
	// Health answers GET /api/v1/health. Required.
	Health HealthChecker
	// Inventory answers the inventory status and refresh endpoints.
	Inventory InventoryReader
	// Containers answers the container list and detail endpoints.
	Containers ContainerReader
	// Warnings supplies per-container inventory warnings.
	Warnings WarningReader
	// Images answers the image endpoints.
	Images ImageReader
	// Networks and Volumes answer their list endpoints.
	Networks NetworkReader
	Volumes  VolumeReader

	// DockerEvents answers the event history endpoints, and EventEngine the
	// status and SSE endpoints. Both nil in a deployment without the event
	// engine, which yields a disabled status rather than a broken route.
	DockerEvents DockerEventReader
	EventEngine  EventEngineReader

	// Snapshots answers the snapshot list and detail endpoints, Capture the
	// POST endpoint, Diffs the comparison endpoint, and Readiness the
	// restore-readiness endpoint. All nil in a deployment with snapshots
	// disabled, which yields a 503 rather than a broken route.
	//
	// There is deliberately no restore capability here, and none anywhere else:
	// Phase 3 records configuration, it does not apply it.
	Snapshots SnapshotReader
	Capture   SnapshotCapturer
	Diffs     SnapshotDiffer
	Readiness SnapshotReadinessEvaluator
	// SnapshotSpecBuilder renders a container's current configuration for a
	// diff against the present, in memory only.
	SnapshotSpecBuilder func(domain.ContainerDetail) domain.SnapshotSpec

	// Logger receives access and error records. Defaults to slog.Default.
	Logger *slog.Logger
	// Config supplies request-size and timeout limits.
	Config config.Server
	// SnapshotConfig supplies the write-endpoint and diff limits.
	SnapshotConfig config.Snapshots

	// Drift answers the drift endpoints. Nil in a deployment with drift
	// detection disabled, which yields a 503 rather than a broken route.
	Drift DriftReader
	// DriftConfig supplies the note length bound for the PATCH endpoint.
	DriftConfig config.Drift

	// Policies answers the policy endpoints and PolicyEngine schedules the
	// manual pass. Both nil in a deployment with the policy engine disabled,
	// which yields a 503 rather than a broken route.
	//
	// This is the first capability in the router that CREATES, UPDATES and
	// WITHDRAWS a record on request. It acts only on HarborMaster's own rows:
	// a policy is a rule configuration is checked against, never one that is
	// applied to Docker.
	Policies     PolicyStore
	PolicyEngine PolicyEvaluator
	// PolicyConfig supplies the definition and note bounds.
	PolicyConfig config.Policy

	// ImageIntel answers the image update endpoints and ImageCollector
	// schedules a metadata collection pass. Both nil in a deployment with image
	// intelligence disabled, which yields a 503 rather than a broken route.
	//
	// Neither carries a registry destination. Nothing an API caller supplies
	// becomes a network request: see image_intel_handlers.go.
	ImageIntel     ImageIntelReader
	ImageCollector ImageIntelCollector

	// Plans answers the change plan endpoints and Planner schedules a
	// generation pass. Both nil in a deployment with planning disabled, which
	// yields a 503 rather than a broken route.
	//
	// "Generate" means generate ANALYSIS. Nothing behind either of these
	// reaches Docker.
	Plans   PlanReader
	Planner PlanGenerator

	// Now is injectable so a status change's timestamp is deterministic in
	// tests. Nil uses the wall clock.
	Now func() time.Time

	// Assets is the compiled frontend. A nil FS yields an API-only server.
	Assets fs.FS
}

// NewServer builds the HTTP handler.
func NewServer(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	s := &Server{
		health:     opts.Health,
		inventory:  opts.Inventory,
		containers: opts.Containers,
		warnings:   opts.Warnings,
		images:     opts.Images,
		networks:   opts.Networks,
		volumes:    opts.Volumes,

		dockerEvents: opts.DockerEvents,
		eventEngine:  opts.EventEngine,

		snapshots:           opts.Snapshots,
		capture:             opts.Capture,
		diffs:               opts.Diffs,
		readiness:           opts.Readiness,
		snapshotSpecBuilder: opts.SnapshotSpecBuilder,

		drift:    opts.Drift,
		driftCfg: opts.DriftConfig,

		policies:     opts.Policies,
		policyEngine: opts.PolicyEngine,
		policyCfg:    opts.PolicyConfig,

		imageIntel:     opts.ImageIntel,
		imageCollector: opts.ImageCollector,

		plans:   opts.Plans,
		planner: opts.Planner,

		now: now,

		logger:      logger,
		cfg:         opts.Config,
		snapshotCfg: opts.SnapshotConfig,
		assets:      opts.Assets,
	}

	s.writeLimiter = newRateLimiter(
		opts.SnapshotConfig.WriteRateLimit,
		opts.SnapshotConfig.WriteRateBurst,
		nil,
	)
	s.policyLimiter = newRateLimiter(
		opts.PolicyConfig.WriteRateLimit,
		opts.PolicyConfig.WriteRateBurst,
		nil,
	)

	s.handler = s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// The API is read-only, so every endpoint is registered twice: the
	// method-qualified pattern serves GET, and the bare path -- more specific
	// than the "/api/" catch-all below -- turns every write attempt into an
	// explicit 405 rather than a misleading 404.
	for path, handler := range map[string]http.HandlerFunc{
		APIPrefix + "/health":    s.handleHealth,
		APIPrefix + "/version":   s.handleVersion,
		APIPrefix + "/inventory": s.handleInventory,
		// Filter vocabularies live under /inventory rather than
		// /containers/filters: a literal path segment there would outrank
		// "/containers/{id}" for non-GET methods, which ServeMux rejects as
		// ambiguous. It is inventory metadata either way.
		APIPrefix + "/inventory/filters":   s.handleContainerFilters,
		APIPrefix + "/containers":          s.handleContainers,
		APIPrefix + "/containers/{id}":     s.handleContainerDetail,
		APIPrefix + "/containers/{id}/raw": s.handleContainerRaw,
		APIPrefix + "/images":              s.handleImages,
		APIPrefix + "/images/{id}":         s.handleImageDetail,
		APIPrefix + "/images/{id}/history": s.handleImageHistory,
		APIPrefix + "/networks":            s.handleNetworks,
		APIPrefix + "/volumes":             s.handleVolumes,
		// Event history. The engine's status lives at /event-engine rather
		// than /events/engine for the same ServeMux reason as
		// /inventory/filters above: a literal "engine" segment registered for
		// all methods would neither contain nor be contained by
		// "GET /events/{id}", which ServeMux rejects as an ambiguous pair.
		APIPrefix + "/events":        s.handleEvents,
		APIPrefix + "/events/{id}":   s.handleEventDetail,
		APIPrefix + "/event-engine":  s.handleEventEngine,
		APIPrefix + "/event-filters": s.handleEventFilters,
		// Configuration snapshots. Read-only, like everything above: the
		// capture endpoint is registered separately below because it is a POST.
		//
		// There is deliberately NO restore, rollback, or apply route. Phase 3
		// records configuration and validates whether it could be restored; it
		// does not restore, and nothing in this router could.
		APIPrefix + "/snapshots/{id}":                   s.handleSnapshotDetail,
		APIPrefix + "/snapshots/{id}/diff":              s.handleSnapshotDiff,
		APIPrefix + "/snapshots/{id}/restore-readiness": s.handleSnapshotReadiness,
	} {
		mux.HandleFunc("GET "+path, handler)
		mux.HandleFunc(path, s.handleMethodNotAllowed("GET, HEAD"))
	}

	// The SSE stream is registered for GET ONLY, with no bare companion.
	//
	// A bare "/events/stream" would match every method on that one path, while
	// "GET /events/{id}" matches GET on every path -- neither contains the
	// other, so ServeMux would panic on the conflicting pair at startup. GET
	// alone is a strict subset of both patterns above, so it resolves cleanly,
	// and a POST to /events/stream still gets an honest 405 from the bare
	// "/events/{id}" handler rather than a 404.
	mux.HandleFunc("GET "+APIPrefix+"/events/stream", s.handleEventStream)

	// The only two non-GET endpoints in the whole router. Neither changes a
	// container: refresh re-reads the host and replaces what HarborMaster has
	// recorded, and capture writes a snapshot to HarborMaster's own database.
	//
	// Both go through guardWrite: strict validation, rate limiting, and the
	// Fetch Metadata checks. See write_guard.go for why the last of those is
	// defence in depth rather than the primary control.
	mux.HandleFunc("POST "+APIPrefix+"/inventory/refresh", s.handleInventoryRefresh)
	mux.HandleFunc(APIPrefix+"/inventory/refresh", s.handleMethodNotAllowed("POST"))

	// /snapshots serves GET for the list and POST for capture, so the bare
	// pattern registers a 405 for everything else.
	mux.HandleFunc("GET "+APIPrefix+"/snapshots", s.handleSnapshots)
	mux.HandleFunc("POST "+APIPrefix+"/snapshots", s.handleSnapshotCreate)
	mux.HandleFunc(APIPrefix+"/snapshots", s.handleMethodNotAllowed("GET, HEAD, POST"))

	// Configuration drift.
	//
	// Four reads and one write. The write moves a record's STATUS on
	// HarborMaster's own row; there is no route here that reaches Docker, and
	// deliberately no evaluate, delete, or remediate endpoint. Drift is
	// evaluated by the background engine on its own schedule, so an
	// unauthenticated caller cannot drive that work on demand.
	mux.HandleFunc("GET "+APIPrefix+"/drift", s.handleDrift)
	mux.HandleFunc(APIPrefix+"/drift", s.handleMethodNotAllowed("GET, HEAD"))

	// GET ONLY, with no bare companion, for the same ServeMux reason as
	// /events/stream above: a bare "/drift/summary" would match every method
	// on that literal while "GET /drift/{id}" matches GET on every segment --
	// neither contains the other, so registering both would panic at startup.
	// A non-GET request to /drift/summary still gets an honest 405 from the
	// bare "/drift/{id}" handler below.
	mux.HandleFunc("GET "+APIPrefix+"/drift/summary", s.handleDriftSummary)

	// Three segments, so this never overlaps the two-segment /drift/{id}.
	mux.HandleFunc("GET "+APIPrefix+"/drift/container/{id}", s.handleDriftByContainer)
	mux.HandleFunc(APIPrefix+"/drift/container/{id}", s.handleMethodNotAllowed("GET, HEAD"))

	mux.HandleFunc("GET "+APIPrefix+"/drift/{id}", s.handleDriftDetail)
	mux.HandleFunc("PATCH "+APIPrefix+"/drift/{id}", s.handleDriftPatch)
	mux.HandleFunc(APIPrefix+"/drift/{id}", s.handleMethodNotAllowed("GET, HEAD, PATCH"))

	// The policy engine.
	//
	// The first routes in HarborMaster that create, update and withdraw a
	// record on request. Every one of them acts on HARBORMASTER'S OWN ROWS: a
	// policy is a rule that configuration is CHECKED AGAINST, never one that is
	// applied to Docker. There is no enforce, no remediate, and no apply route,
	// and nothing here can reach the daemon.
	mux.HandleFunc("GET "+APIPrefix+"/policies", s.handlePolicies)
	mux.HandleFunc("POST "+APIPrefix+"/policies", s.handlePolicyCreate)
	mux.HandleFunc(APIPrefix+"/policies", s.handleMethodNotAllowed("GET, HEAD, POST"))

	mux.HandleFunc("GET "+APIPrefix+"/policies/{id}", s.handlePolicyDetail)
	mux.HandleFunc("PATCH "+APIPrefix+"/policies/{id}", s.handlePolicyUpdate)
	// DELETE ARCHIVES. A violation references its policy and the history must
	// survive the rule being withdrawn, so the row is retained and its open
	// violations resolve. See policy_handlers.go.
	mux.HandleFunc("DELETE "+APIPrefix+"/policies/{id}", s.handlePolicyDelete)
	mux.HandleFunc(APIPrefix+"/policies/{id}", s.handleMethodNotAllowed("GET, HEAD, PATCH, DELETE"))

	// The rule catalogue, so the policy editor is built from the same source of
	// truth the validator uses.
	mux.HandleFunc("GET "+APIPrefix+"/policy-rules", s.handlePolicyRules)
	mux.HandleFunc(APIPrefix+"/policy-rules", s.handleMethodNotAllowed("GET, HEAD"))

	mux.HandleFunc("GET "+APIPrefix+"/policy-summary", s.handlePolicySummary)
	mux.HandleFunc(APIPrefix+"/policy-summary", s.handleMethodNotAllowed("GET, HEAD"))

	mux.HandleFunc("GET "+APIPrefix+"/policy-violations", s.handlePolicyViolations)
	mux.HandleFunc(APIPrefix+"/policy-violations", s.handleMethodNotAllowed("GET, HEAD"))

	// Three segments, so this never overlaps the two-segment
	// /policy-violations/{id} below.
	mux.HandleFunc("GET "+APIPrefix+"/policy-violations/container/{id}", s.handlePolicyByContainer)
	mux.HandleFunc(APIPrefix+"/policy-violations/container/{id}", s.handleMethodNotAllowed("GET, HEAD"))

	mux.HandleFunc("GET "+APIPrefix+"/policy-violations/{id}", s.handlePolicyViolationDetail)
	mux.HandleFunc("PATCH "+APIPrefix+"/policy-violations/{id}", s.handlePolicyViolationPatch)
	mux.HandleFunc(APIPrefix+"/policy-violations/{id}", s.handleMethodNotAllowed("GET, HEAD, PATCH"))

	// The manual pass. ASYNCHRONOUS: it schedules work through the same
	// coalescing queue the scheduled passes use, so calling it in a loop
	// produces one pass rather than a backlog, and no caller can hold a
	// request open across a thousand-container sweep.
	mux.HandleFunc("POST "+APIPrefix+"/policy/evaluate", s.handlePolicyEvaluate)
	mux.HandleFunc(APIPrefix+"/policy/evaluate", s.handleMethodNotAllowed("POST"))

	// Image intelligence.
	//
	// Three reads and one write, and the write SCHEDULES a metadata collection
	// pass -- it pulls nothing, changes no container, and takes no target of any
	// kind. There is deliberately no endpoint that applies an update, and no
	// parameter anywhere in this feature that becomes a network destination.
	//
	// GET ONLY, with no bare companion, for the same ServeMux reason as
	// /events/stream: a bare "/images/updates" would match every method on that
	// literal while "GET /images/{id}" matches GET on every segment -- neither
	// contains the other, so registering both would panic at startup. A non-GET
	// request to /images/updates still gets an honest 405 from the bare
	// "/images/{id}" handler registered above.
	mux.HandleFunc("GET "+APIPrefix+"/images/updates", s.handleImageUpdates)

	// POST /images/refresh is strictly more specific than the bare
	// "/images/{id}" pattern, so this pair resolves without ambiguity.
	mux.HandleFunc("POST "+APIPrefix+"/images/refresh", s.handleImageRefresh)

	// Change planning.
	//
	// Three reads and one write, and the write generates HarborMaster's own
	// ANALYSIS of its own database. It pulls nothing, changes no container, and
	// schedules no change; there is deliberately no route that applies a plan,
	// and no PATCH or DELETE, because a plan is immutable evidence of what was
	// known at one moment.
	mux.HandleFunc("GET "+APIPrefix+"/plans", s.handlePlans)
	mux.HandleFunc(APIPrefix+"/plans", s.handleMethodNotAllowed("GET, HEAD"))

	// Three segments, so this never overlaps the two-segment /plans/{id}.
	mux.HandleFunc("GET "+APIPrefix+"/plans/container/{id}", s.handlePlansByContainer)
	mux.HandleFunc(APIPrefix+"/plans/container/{id}", s.handleMethodNotAllowed("GET, HEAD"))

	// POST /plans/generate is strictly more specific than the bare
	// "/plans/{id}" pattern below, so this pair resolves without ambiguity.
	mux.HandleFunc("POST "+APIPrefix+"/plans/generate", s.handlePlanGenerate)

	mux.HandleFunc("GET "+APIPrefix+"/plans/{id}", s.handlePlanDetail)
	mux.HandleFunc(APIPrefix+"/plans/{id}", s.handleMethodNotAllowed("GET, HEAD"))

	// Any other /api/ path is a JSON 404, never the SPA shell.
	mux.HandleFunc("/api/", s.handleAPINotFound)

	// Everything else is the frontend.
	mux.Handle("/", newStaticHandler(s.assets, s.logger))

	maxBody := s.cfg.MaxRequestBytes
	if maxBody <= 0 {
		maxBody = config.DefaultMaxRequestBytes
	}

	return chain(mux,
		withRequestID,
		withRecovery(s.logger),
		withAccessLog(s.logger),
		withSecurityHeaders,
		withMaxBody(maxBody, s.logger),
	)
}

// HTTPServer returns an *http.Server carrying the configured timeouts.
//
// Every timeout is set explicitly: a server with no deadlines can be held open
// indefinitely by a slow or hostile client.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 16, // 64 KiB
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
	}
}
