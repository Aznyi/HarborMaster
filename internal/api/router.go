package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sync/atomic"
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

	// acquisitions answers the image acquisition endpoints -- the only ones in
	// this API that can change the Docker host. Nil in a deployment that has
	// not opted in, which yields a 503 rather than a broken route.
	acquisitions AcquisitionService
	// executions answers the container recreation endpoints -- the only ones in
	// this API that change something that is RUNNING. Nil in a deployment that
	// has not opted in, which yields a 503 rather than a broken route.
	executions ExecutionService
	// rollbacks answers the rollback endpoints. Nil in a deployment with
	// rollback disabled, which yields 503 rather than a broken route.
	rollbacks RollbackService

	// automation answers the update-engine endpoints -- the only ones in this
	// API that can cause a host change nobody is watching. Nil in a deployment
	// that has not opted in, which yields a 503 rather than a broken route.
	automation AutomationService
	// updatePolicies answers the automation-rule endpoints. Separate from
	// `policies` above, which is compliance: one reports, the other acts, and
	// conflating them would let one edit turn a reporting rule into a mutation
	// rule.
	updatePolicies UpdatePolicyService
	// notifications answers the delivery-history endpoints, and
	// notificationAdmin the configuration ones. Separate for the reason the two
	// automation fields are: reading what was sent is a permission every role
	// holds, and configuring where things are sent is an administrator's.
	notifications     NotificationReader
	notificationAdmin NotificationAdmin

	// auth resolves sessions and answers the authentication endpoints. A nil
	// auth serves the public routes and refuses everything else with 503 --
	// fail closed, because a misconfiguration must never silently restore
	// anonymous access to a Docker mutation.
	auth AuthService
	// users answers the account administration endpoints, and audit records and
	// serves the security log. Both nil in a server built without them.
	users UserService
	audit AuditService
	// authCfg carries the cookie and session settings.
	authCfg config.Auth
	// proxies is the parsed TRUSTED_PROXIES allowlist. Empty by default, which
	// means forwarding headers are ignored entirely.
	proxies *trustedProxies
	// claimed caches "this installation has an administrator".
	//
	// One-way: it only ever flips false to true. The value changes exactly once
	// in an installation's life, so a database round trip per request to
	// re-learn it would be waste -- and because the cache cannot flip back, a
	// stale read can delay noticing the bootstrap but can never re-open it.
	claimed atomic.Bool

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

	// Acquisitions is the image acquisition capability. Nil disables the
	// endpoints entirely, which is the correct behaviour for a deployment that
	// has not asked for the ability to write to its Docker host.
	Acquisitions AcquisitionService

	// Executions is the container recreation capability. Nil disables the
	// endpoints entirely, which is the correct behaviour for a deployment that
	// has not asked for the ability to stop and replace its containers.
	//
	// Nothing behind this can be aimed: the request body carries an acquisition
	// id and nothing else, and the service revalidates every prerequisite
	// before touching Docker.
	Executions ExecutionService
	// Rollbacks answers the rollback endpoints -- returning a container to the
	// state a recreation moved it from. Nil disables them.
	//
	// Note what this interface CANNOT reach: the rollback service holds the
	// Docker capability, and an architecture test fails the build if this
	// package names it.
	Rollbacks RollbackService

	// Automation answers the update-engine endpoints. Nil disables them
	// entirely, which is the correct behaviour for a deployment that has not
	// asked for unattended updates.
	//
	// Note what this interface CANNOT reach: the engine holds no Docker
	// capability at all, and an architecture test fails the build if the
	// automation sources so much as name one.
	Automation AutomationService
	// UpdatePolicies answers the automation-rule endpoints. Nil disables them.
	//
	// Reachable even when the ENGINE is off, deliberately: an operator must be
	// able to write and review their rules before switching automation on,
	// which is the order those two things should be done in.
	UpdatePolicies UpdatePolicyService
	// Notifications answers the delivery-history endpoints. Nil disables them.
	//
	// Non-nil even on a deployment with sending switched off: the history and
	// the destination list stay readable so an administrator can configure and
	// review before turning delivery on.
	Notifications NotificationReader
	// NotificationAdmin answers the destination and rule endpoints. Nil
	// disables them.
	NotificationAdmin NotificationAdmin

	// Auth resolves sessions and answers the authentication endpoints.
	//
	// A server built WITHOUT it serves the four public routes and refuses
	// everything else with 503. That is the fail-closed behaviour and it is
	// worth being explicit about: there is no configuration, and no partial
	// wiring, that restores anonymous access to an authenticated endpoint.
	Auth AuthService
	// Users answers account administration, and Audit records and serves the
	// security log.
	Users UserService
	Audit AuditService
	// AuthConfig supplies the cookie, session, and trusted-proxy settings.
	AuthConfig config.Auth

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

		acquisitions: opts.Acquisitions,
		executions:   opts.Executions,
		rollbacks:    opts.Rollbacks,

		automation:        opts.Automation,
		updatePolicies:    opts.UpdatePolicies,
		notifications:     opts.Notifications,
		notificationAdmin: opts.NotificationAdmin,

		auth:    opts.Auth,
		users:   opts.Users,
		audit:   opts.Audit,
		authCfg: opts.AuthConfig,
		proxies: newTrustedProxies(opts.AuthConfig.TrustedProxies),

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

// routes builds the HTTP handler from the route table.
//
// # Every registration goes through guard
//
// There is no path through this function that registers a bare handler. The
// mux only ever receives a handler that has already been wrapped with its
// access policy, which is what makes "no route without a policy" a property of
// the code rather than a convention.
//
// # The middleware chain is unchanged
//
// Authentication is per-route rather than a link in this chain, deliberately:
// a chain link would have to re-derive which route a request is going to hit,
// and two matchers that can disagree is how an endpoint ends up unprotected.
// See internal/api/auth_middleware.go.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	for _, entry := range s.routeTable() {
		handler := entry.handler
		if entry.method == "" {
			// The bare pattern. It answers 405 for a path that exists but does
			// not serve this method, which is more useful than the 404 the
			// "/api/" catch-all would otherwise give -- and it carries the
			// path's read policy, so an unauthenticated caller still gets 401
			// and learns nothing about which paths exist.
			access := entry.access
			access.methodNotAllowed = true
			mux.HandleFunc(entry.pattern, s.guard(access,
				s.handleMethodNotAllowed(entry.allowed)))
			continue
		}
		mux.HandleFunc(entry.method+" "+entry.pattern, s.guard(entry.access, handler))
	}

	// Any other /api/ path is a JSON 404, never the SPA shell. Public because
	// "this path does not exist" discloses nothing, and because answering 401
	// here would tell a scanner that everything it tried was real.
	mux.HandleFunc("/api/", s.handleAPINotFound)

	// Everything else is the frontend.
	//
	// # Why the SPA bundle is served without a session
	//
	// It contains no data. It is a static JavaScript bundle whose first act is
	// to call GET /auth/session, which is authenticated -- so an unauthenticated
	// visitor gets the login page and nothing else. Requiring a session to
	// fetch the bundle would mean there was no page to log in FROM.
	//
	// Every byte of estate data comes from the API, and the API is protected.
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
