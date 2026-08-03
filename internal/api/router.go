package api

import (
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/config"
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

	logger  *slog.Logger
	cfg     config.Server
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

	// Logger receives access and error records. Defaults to slog.Default.
	Logger *slog.Logger
	// Config supplies request-size and timeout limits.
	Config config.Server
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
		logger:     logger,
		cfg:        opts.Config,
		assets:     opts.Assets,
	}
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
	} {
		mux.HandleFunc("GET "+path, handler)
		mux.HandleFunc(path, s.handleMethodNotAllowed("GET, HEAD"))
	}

	// The one non-GET endpoint in the whole router. It mutates HarborMaster's
	// own inventory, never Docker: it re-reads the host and replaces what
	// HarborMaster has recorded. Nothing here can change a container.
	mux.HandleFunc("POST "+APIPrefix+"/inventory/refresh", s.handleInventoryRefresh)
	mux.HandleFunc(APIPrefix+"/inventory/refresh", s.handleMethodNotAllowed("POST"))

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
