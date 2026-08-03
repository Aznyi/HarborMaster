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
	health  HealthChecker
	logger  *slog.Logger
	cfg     config.Server
	assets  fs.FS
	handler http.Handler
}

// Options configures a Server.
type Options struct {
	// Health answers GET /api/v1/health. Required.
	Health HealthChecker
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
		health: opts.Health,
		logger: logger,
		cfg:    opts.Config,
		assets: opts.Assets,
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
		APIPrefix + "/health":  s.handleHealth,
		APIPrefix + "/version": s.handleVersion,
	} {
		mux.HandleFunc("GET "+path, handler)
		mux.HandleFunc(path, s.handleMethodNotAllowed("GET, HEAD"))
	}

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
