package api

import (
	"io/fs"
	"log/slog"
	"net/http"

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
	// snapshotSpecBuilder renders a container's CURRENT configuration as a
	// canonical document, in memory, for a diff against the present. It never
	// persists anything: a GET must not create a durable record.
	snapshotSpecBuilder func(domain.ContainerDetail) domain.SnapshotSpec

	logger *slog.Logger
	cfg    config.Server
	// snapshotCfg carries the write-endpoint and diff limits.
	snapshotCfg config.Snapshots
	// writeLimiter bounds the two POST endpoints. Per process, not per client:
	// there is no authentication and therefore no trustworthy client identity.
	writeLimiter *rateLimiter

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
	// Assets is the compiled frontend. A nil FS yields an API-only server.
	Assets fs.FS
}

// NewServer builds the HTTP handler.
func NewServer(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
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
