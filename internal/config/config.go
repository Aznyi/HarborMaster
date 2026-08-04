// Package config loads HarborMaster's runtime configuration from the
// environment.
//
// Two rules govern this package:
//
//   - Defaults are safe. The HTTP listener binds to loopback so that running
//     the bare binary never exposes the API to the network by accident. The
//     container image opts in to 0.0.0.0 explicitly.
//   - Values are never logged. Config is a plausible carrier for secrets in
//     later work (registry credentials, notification webhooks), so the
//     redacted String method is the only rendering this package offers.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

const envPrefix = "HARBORMASTER_"

// Default values, also documented in .env.example.
const (
	DefaultHTTPAddr          = "127.0.0.1:8080"
	DefaultMaxRequestBytes   = int64(1 << 20) // 1 MiB
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 15 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
	DefaultDockerTimeout     = 10 * time.Second
	DefaultDBPath            = "./data/harbormaster.db"
	DefaultLogLevel          = "info"
	DefaultLogFormat         = "json"

	// Inventory defaults.
	//
	// DefaultRefreshInterval is a compromise: often enough that the UI is not
	// stale, rare enough that HarborMaster is not a load source on a host with
	// a thousand containers. Set the interval to 0 to disable periodic refresh
	// and drive it manually.
	DefaultInventoryEnabled = true
	DefaultRefreshOnStartup = true
	DefaultRefreshInterval  = 60 * time.Second
	DefaultInventoryWorkers = 8
	DefaultAbsentRetention  = 7 * 24 * time.Hour
	MinRefreshInterval      = 5 * time.Second
	MaxInventoryWorkers     = 64

	// Event engine defaults.
	//
	// DefaultEventReconcileInterval is deliberately much longer than
	// DefaultRefreshInterval. With live events driving targeted refreshes, a
	// full sweep is a safety net for what the stream missed rather than the
	// primary way the inventory stays current. See Config.ReconcileInterval for
	// how the two settings relate.
	DefaultEventsEnabled          = true
	DefaultEventReconnectInitial  = 1 * time.Second
	DefaultEventReconnectMax      = 60 * time.Second
	DefaultEventReconnectFactor   = 2.0
	DefaultEventBufferSize        = 1024
	DefaultEventBatchSize         = 64
	DefaultEventBatchFlush        = 500 * time.Millisecond
	DefaultEventDedupWindow       = 10 * time.Second
	DefaultEventRefreshDebounce   = 750 * time.Millisecond
	DefaultEventReconcileInterval = 15 * time.Minute
	DefaultEventRetentionAge      = 7 * 24 * time.Hour
	DefaultEventRetentionCount    = int64(50000)
	DefaultEventPruneInterval     = 1 * time.Hour
	DefaultEventStreamSubscribers = 16
	DefaultEventStreamBuffer      = 128
	DefaultEventStreamReplay      = 200
	DefaultEventStreamHeartbeat   = 20 * time.Second

	// Event engine bounds. The minimums exist for the same reason
	// MinRefreshInterval does: a tiny value would hammer a privileged socket or
	// spin a goroutine, and is far more likely to be a typo than an intention.
	MinEventReconnectDelay    = 100 * time.Millisecond
	MinEventDedupWindow       = 1 * time.Second
	MinEventReconcileInterval = 30 * time.Second
	MinEventPruneInterval     = 1 * time.Minute
	MinEventStreamHeartbeat   = 1 * time.Second
	MaxEventBufferSize        = 1 << 16
	MaxEventBatchSize         = 1024
	MaxEventStreamSubscribers = 256
	MaxEventStreamReplay      = 1000

	// DefaultHealthcheckTimeout bounds the `harbormaster healthcheck` probe.
	// It is deliberately short: a container health check that outlives the
	// orchestrator's own timeout is worse than useless, because the runtime
	// kills it and reports a failure regardless of the application's state.
	DefaultHealthcheckTimeout = 3 * time.Second

	unixDockerHost    = "unix:///var/run/docker.sock"
	windowsDockerHost = "npipe:////./pipe/docker_engine"
)

// Server holds HTTP listener settings.
type Server struct {
	Addr              string
	MaxRequestBytes   int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// Docker holds Docker Engine connection settings.
type Docker struct {
	// Host is the Engine endpoint, e.g. unix:///var/run/docker.sock.
	Host string
	// Timeout bounds a single Engine API call.
	Timeout time.Duration
}

// Store holds persistence settings.
type Store struct {
	// Path is the SQLite database file.
	Path string
}

// Inventory holds settings for the read-only inventory engine.
type Inventory struct {
	// Enabled turns the whole inventory engine on or off. When false no
	// refresh runs and the API serves whatever was last persisted.
	Enabled bool
	// RefreshOnStartup runs one refresh as soon as the server is up.
	RefreshOnStartup bool
	// RefreshInterval is the periodic refresh period. Zero disables periodic
	// refresh; manual refresh still works.
	RefreshInterval time.Duration
	// Workers bounds concurrent container inspections. The Docker socket is a
	// single daemon, so more is not automatically faster, and unbounded
	// concurrency against 1,000 containers would be hostile to the host.
	Workers int
	// MaskPatterns are the environment-variable name fragments treated as
	// secret-bearing.
	MaskPatterns []string
	// AbsentRetention is how long a container that has disappeared is kept in
	// the inventory before being purged. Zero keeps absent containers forever.
	AbsentRetention time.Duration
}

// Events holds settings for the read-only Docker event engine.
//
// The engine subscribes to the daemon's event stream and uses events as HINTS
// that inventory may have changed. It never writes container state from an
// event: it asks the inventory service to re-read the resource, so there stays
// exactly one normalization and persistence path.
type Events struct {
	// Enabled turns the whole event engine on or off. When false nothing
	// connects to the event stream, the SSE endpoint reports the feature
	// disabled, and periodic inventory refresh carries the inventory alone.
	Enabled bool

	// ReconnectInitial is the first backoff delay after a stream drop, and
	// ReconnectMax caps it. ReconnectFactor multiplies the delay after each
	// consecutive failure. Jitter is applied on top so a daemon restart does
	// not produce a synchronised reconnect storm across several HarborMasters.
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	ReconnectFactor  float64

	// BufferSize bounds the in-process queue between the stream reader and the
	// event processor. The reader never blocks on it: when it is full the event
	// is dropped, counted, and a full reconciliation is requested, because a
	// blocked reader would silently stall the whole stream.
	BufferSize int
	// BatchSize and BatchFlush bound how many events are persisted in one
	// transaction and how long a partial batch waits.
	BatchSize  int
	BatchFlush time.Duration

	// DedupWindow is how long an event fingerprint is remembered in memory. A
	// genuinely repeated event with a different Docker timestamp has a
	// different fingerprint and is NOT suppressed.
	DedupWindow time.Duration

	// RefreshDebounce coalesces the burst of events one lifecycle transition
	// produces (die, stop, start, health_status) into a single refresh per
	// resource.
	RefreshDebounce time.Duration

	// ReconcileInterval is the periodic full-reconciliation period. See
	// Config.ReconcileInterval for how it interacts with the inventory's own
	// refresh interval.
	ReconcileInterval time.Duration

	// RetentionAge and RetentionCount bound stored event history. Zero on
	// either disables that dimension of pruning; both zero keeps everything,
	// which is a valid but unbounded choice.
	RetentionAge   time.Duration
	RetentionCount int64
	// PruneInterval is how often retention runs.
	PruneInterval time.Duration

	// StreamSubscribers caps concurrent SSE clients. StreamBuffer bounds each
	// subscriber's queue: a slow reader drops events rather than blocking event
	// processing for everyone else.
	StreamSubscribers int
	StreamBuffer      int
	// StreamReplay caps how many stored events a Last-Event-ID reconnect
	// replays. A client that fell far behind gets a truncation notice and
	// should reload the paginated history instead.
	StreamReplay int
	// StreamHeartbeat is the comment-frame interval that keeps an idle stream
	// alive through proxies.
	StreamHeartbeat time.Duration
}

// Healthcheck holds settings for the `harbormaster healthcheck` command.
type Healthcheck struct {
	// Timeout bounds the whole probe, connection included.
	Timeout time.Duration
}

// Log holds structured-logging settings.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is one of json, text.
	Format string
}

// Config is the fully resolved application configuration.
type Config struct {
	Server      Server
	Docker      Docker
	Store       Store
	Log         Log
	Healthcheck Healthcheck
	Inventory   Inventory
	Events      Events
}

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"json", "text"}
)

// Load reads configuration from the process environment, applying defaults for
// anything unset and validating the result.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// lookupFunc mirrors os.LookupEnv so tests can supply a fake environment
// without mutating the real one.
type lookupFunc func(string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := Config{
		Server: Server{
			Addr: stringVar(lookup, "HTTP_ADDR", DefaultHTTPAddr),
		},
		Docker: Docker{
			Host: stringVar(lookup, "DOCKER_HOST", defaultDockerHost()),
		},
		Store: Store{
			Path: stringVar(lookup, "DB_PATH", DefaultDBPath),
		},
		Log: Log{
			Level:  strings.ToLower(stringVar(lookup, "LOG_LEVEL", DefaultLogLevel)),
			Format: strings.ToLower(stringVar(lookup, "LOG_FORMAT", DefaultLogFormat)),
		},
		Inventory: Inventory{
			MaskPatterns: listVar(lookup, "INVENTORY_MASK_PATTERNS", domain.DefaultMaskPatterns),
		},
	}

	var err error

	cfg.Server.MaxRequestBytes, err = int64Var(lookup, "MAX_REQUEST_BYTES", DefaultMaxRequestBytes)
	collect(err)
	cfg.Server.ReadHeaderTimeout, err = durationVar(lookup, "READ_HEADER_TIMEOUT", DefaultReadHeaderTimeout)
	collect(err)
	cfg.Server.ReadTimeout, err = durationVar(lookup, "READ_TIMEOUT", DefaultReadTimeout)
	collect(err)
	cfg.Server.WriteTimeout, err = durationVar(lookup, "WRITE_TIMEOUT", DefaultWriteTimeout)
	collect(err)
	cfg.Server.IdleTimeout, err = durationVar(lookup, "IDLE_TIMEOUT", DefaultIdleTimeout)
	collect(err)
	cfg.Server.ShutdownTimeout, err = durationVar(lookup, "SHUTDOWN_TIMEOUT", DefaultShutdownTimeout)
	collect(err)
	cfg.Docker.Timeout, err = durationVar(lookup, "DOCKER_TIMEOUT", DefaultDockerTimeout)
	collect(err)
	cfg.Healthcheck.Timeout, err = durationVar(lookup, "HEALTHCHECK_TIMEOUT", DefaultHealthcheckTimeout)
	collect(err)

	cfg.Inventory.Enabled, err = boolVar(lookup, "INVENTORY_ENABLED", DefaultInventoryEnabled)
	collect(err)
	cfg.Inventory.RefreshOnStartup, err = boolVar(lookup, "INVENTORY_REFRESH_ON_STARTUP", DefaultRefreshOnStartup)
	collect(err)
	cfg.Inventory.RefreshInterval, err = durationVar(lookup, "INVENTORY_REFRESH_INTERVAL", DefaultRefreshInterval)
	collect(err)
	cfg.Inventory.AbsentRetention, err = durationVar(lookup, "INVENTORY_ABSENT_RETENTION", DefaultAbsentRetention)
	collect(err)

	workers, err := int64Var(lookup, "INVENTORY_WORKERS", int64(DefaultInventoryWorkers))
	collect(err)
	cfg.Inventory.Workers = int(workers)

	cfg.Events.Enabled, err = boolVar(lookup, "EVENTS_ENABLED", DefaultEventsEnabled)
	collect(err)
	cfg.Events.ReconnectInitial, err = durationVar(lookup, "EVENTS_RECONNECT_INITIAL_DELAY", DefaultEventReconnectInitial)
	collect(err)
	cfg.Events.ReconnectMax, err = durationVar(lookup, "EVENTS_RECONNECT_MAX_DELAY", DefaultEventReconnectMax)
	collect(err)
	cfg.Events.ReconnectFactor, err = floatVar(lookup, "EVENTS_RECONNECT_MULTIPLIER", DefaultEventReconnectFactor)
	collect(err)
	cfg.Events.BatchFlush, err = durationVar(lookup, "EVENTS_BATCH_FLUSH_INTERVAL", DefaultEventBatchFlush)
	collect(err)
	cfg.Events.DedupWindow, err = durationVar(lookup, "EVENTS_DEDUP_WINDOW", DefaultEventDedupWindow)
	collect(err)
	cfg.Events.RefreshDebounce, err = durationVar(lookup, "EVENTS_REFRESH_DEBOUNCE", DefaultEventRefreshDebounce)
	collect(err)
	cfg.Events.ReconcileInterval, err = durationVar(lookup, "EVENTS_RECONCILE_INTERVAL", DefaultEventReconcileInterval)
	collect(err)
	cfg.Events.RetentionAge, err = durationVar(lookup, "EVENTS_RETENTION_AGE", DefaultEventRetentionAge)
	collect(err)
	cfg.Events.PruneInterval, err = durationVar(lookup, "EVENTS_PRUNE_INTERVAL", DefaultEventPruneInterval)
	collect(err)
	cfg.Events.StreamHeartbeat, err = durationVar(lookup, "EVENTS_STREAM_HEARTBEAT", DefaultEventStreamHeartbeat)
	collect(err)

	cfg.Events.RetentionCount, err = int64Var(lookup, "EVENTS_RETENTION_COUNT", DefaultEventRetentionCount)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"EVENTS_BUFFER_SIZE", DefaultEventBufferSize, &cfg.Events.BufferSize},
		{"EVENTS_BATCH_SIZE", DefaultEventBatchSize, &cfg.Events.BatchSize},
		{"EVENTS_STREAM_MAX_SUBSCRIBERS", DefaultEventStreamSubscribers, &cfg.Events.StreamSubscribers},
		{"EVENTS_STREAM_BUFFER_SIZE", DefaultEventStreamBuffer, &cfg.Events.StreamBuffer},
		{"EVENTS_STREAM_REPLAY_LIMIT", DefaultEventStreamReplay, &cfg.Events.StreamReplay},
	} {
		value, convErr := int64Var(lookup, target.name, int64(target.fallback))
		collect(convErr)
		*target.into = int(value)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the configuration is internally usable.
func (c Config) Validate() error {
	var errs []error

	if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		errs = append(errs, fmt.Errorf("%sHTTP_ADDR must be host:port", envPrefix))
	}
	if c.Server.MaxRequestBytes <= 0 {
		errs = append(errs, fmt.Errorf("%sMAX_REQUEST_BYTES must be positive", envPrefix))
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"READ_HEADER_TIMEOUT", c.Server.ReadHeaderTimeout},
		{"READ_TIMEOUT", c.Server.ReadTimeout},
		{"WRITE_TIMEOUT", c.Server.WriteTimeout},
		{"IDLE_TIMEOUT", c.Server.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", c.Server.ShutdownTimeout},
		{"DOCKER_TIMEOUT", c.Docker.Timeout},
		{"HEALTHCHECK_TIMEOUT", c.Healthcheck.Timeout},
	} {
		if d.value <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive", envPrefix, d.name))
		}
	}
	if strings.TrimSpace(c.Docker.Host) == "" {
		errs = append(errs, fmt.Errorf("%sDOCKER_HOST must not be empty", envPrefix))
	}
	if strings.TrimSpace(c.Store.Path) == "" {
		errs = append(errs, fmt.Errorf("%sDB_PATH must not be empty", envPrefix))
	}
	if !slices.Contains(validLogLevels, c.Log.Level) {
		errs = append(errs, fmt.Errorf("%sLOG_LEVEL must be one of %s", envPrefix, strings.Join(validLogLevels, ", ")))
	}
	if !slices.Contains(validLogFormats, c.Log.Format) {
		errs = append(errs, fmt.Errorf("%sLOG_FORMAT must be one of %s", envPrefix, strings.Join(validLogFormats, ", ")))
	}

	// A zero interval is a valid, documented way to disable periodic refresh.
	// A tiny non-zero one is almost always a mistake, and it would hammer a
	// privileged socket, so it is rejected rather than silently clamped.
	if c.Inventory.RefreshInterval != 0 && c.Inventory.RefreshInterval < MinRefreshInterval {
		errs = append(errs, fmt.Errorf("%sINVENTORY_REFRESH_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinRefreshInterval))
	}
	if c.Inventory.Workers < 1 || c.Inventory.Workers > MaxInventoryWorkers {
		errs = append(errs, fmt.Errorf("%sINVENTORY_WORKERS must be between 1 and %d",
			envPrefix, MaxInventoryWorkers))
	}
	if c.Inventory.AbsentRetention < 0 {
		errs = append(errs, fmt.Errorf("%sINVENTORY_ABSENT_RETENTION must not be negative", envPrefix))
	}

	errs = append(errs, c.Events.validate()...)

	return errors.Join(errs...)
}

// validate checks the event-engine settings.
//
// The settings are validated even when the engine is disabled. A configuration
// error that only surfaces the day someone flips EVENTS_ENABLED to true is a
// worse failure than one caught at startup.
func (e Events) validate() []error {
	var errs []error

	positiveDurations := []struct {
		name  string
		value time.Duration
		min   time.Duration
	}{
		{"EVENTS_RECONNECT_INITIAL_DELAY", e.ReconnectInitial, MinEventReconnectDelay},
		{"EVENTS_RECONNECT_MAX_DELAY", e.ReconnectMax, MinEventReconnectDelay},
		{"EVENTS_BATCH_FLUSH_INTERVAL", e.BatchFlush, time.Millisecond},
		{"EVENTS_DEDUP_WINDOW", e.DedupWindow, MinEventDedupWindow},
		{"EVENTS_REFRESH_DEBOUNCE", e.RefreshDebounce, time.Millisecond},
		{"EVENTS_RECONCILE_INTERVAL", e.ReconcileInterval, MinEventReconcileInterval},
		{"EVENTS_PRUNE_INTERVAL", e.PruneInterval, MinEventPruneInterval},
		{"EVENTS_STREAM_HEARTBEAT", e.StreamHeartbeat, MinEventStreamHeartbeat},
	}
	for _, d := range positiveDurations {
		if d.value < d.min {
			errs = append(errs, fmt.Errorf("%s%s must be at least %s", envPrefix, d.name, d.min))
		}
	}

	// A maximum below the initial delay would make the backoff shrink on the
	// first failure, which is the opposite of what backoff is for.
	if e.ReconnectMax > 0 && e.ReconnectInitial > 0 && e.ReconnectMax < e.ReconnectInitial {
		errs = append(errs, fmt.Errorf(
			"%sEVENTS_RECONNECT_MAX_DELAY must not be smaller than %sEVENTS_RECONNECT_INITIAL_DELAY",
			envPrefix, envPrefix))
	}
	// A multiplier of exactly 1 is a fixed-interval retry, which is allowed;
	// below 1 the delay would decay towards zero.
	if e.ReconnectFactor < 1 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RECONNECT_MULTIPLIER must be at least 1", envPrefix))
	}

	bounded := []struct {
		name            string
		value, min, max int
	}{
		{"EVENTS_BUFFER_SIZE", e.BufferSize, 1, MaxEventBufferSize},
		{"EVENTS_BATCH_SIZE", e.BatchSize, 1, MaxEventBatchSize},
		{"EVENTS_STREAM_MAX_SUBSCRIBERS", e.StreamSubscribers, 0, MaxEventStreamSubscribers},
		{"EVENTS_STREAM_BUFFER_SIZE", e.StreamBuffer, 1, MaxEventBufferSize},
		{"EVENTS_STREAM_REPLAY_LIMIT", e.StreamReplay, 0, MaxEventStreamReplay},
	}
	for _, b := range bounded {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	// Zero is the documented way to disable a retention dimension. Negative is
	// not a way to do anything.
	if e.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RETENTION_AGE must not be negative", envPrefix))
	}
	if e.RetentionCount < 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RETENTION_COUNT must not be negative", envPrefix))
	}
	// Batching more than the queue holds cannot help and reads as a mistake.
	if e.BatchSize > e.BufferSize && e.BufferSize > 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_BATCH_SIZE must not exceed %sEVENTS_BUFFER_SIZE",
			envPrefix, envPrefix))
	}

	return errs
}

// ReconcileInterval reports the period of the ONE periodic full-inventory
// sweep, together with which component owns it.
//
// Exactly one full-refresh timer may run. Two would double the load on a
// privileged socket and make "when was the last sweep" ambiguous. So:
//
//   - Event engine enabled: the engine owns reconciliation at
//     EVENTS_RECONCILE_INTERVAL, and the inventory service's own ticker is
//     suppressed. Targeted, event-driven refreshes carry the inventory between
//     sweeps, so the sweep is a safety net rather than the main mechanism.
//   - Event engine disabled: nothing changes from Phase 2. The inventory
//     service keeps its own INVENTORY_REFRESH_INTERVAL ticker, including the
//     documented "0 disables it" behaviour.
//
// INVENTORY_REFRESH_INTERVAL is therefore retained, not removed: an existing
// configuration keeps working, and it still governs the no-events case.
func (c Config) ReconcileInterval() (interval time.Duration, ownedByEventEngine bool) {
	if c.Events.Enabled && c.Inventory.Enabled {
		return c.Events.ReconcileInterval, true
	}
	return c.Inventory.RefreshInterval, false
}

// String renders the configuration for logs WITHOUT any environment-variable
// values. It reports only which knobs exist, so an operator can confirm the
// process is configured at all without the log becoming a disclosure channel.
//
// Do not add value interpolation here.
func (c Config) String() string {
	return "config{redacted: server, docker, store, log, healthcheck, inventory, events}"
}

// Masker builds the environment masker from the configured patterns.
func (c Config) Masker() *domain.Masker {
	return domain.NewMasker(c.Inventory.MaskPatterns)
}

// IsLoopback reports whether the HTTP listener is bound to a loopback address.
// The server logs a warning when it is not, since HarborMaster fronts a
// privileged Docker socket.
func (c Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Server.Addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// HealthcheckURL returns the URL of this process's own health endpoint.
//
// It is derived from the listen address rather than configured separately, so
// the container health check cannot drift out of sync with the port the server
// actually binds. A wildcard bind is rewritten to loopback: the probe runs
// inside the same network namespace, and dialling 0.0.0.0 is not portable.
func (c Config) HealthcheckURL() string {
	host, port, err := net.SplitHostPort(c.Server.Addr)
	if err != nil || port == "" {
		return "http://127.0.0.1:8080" + healthPath
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	// net.JoinHostPort adds the brackets an IPv6 literal needs in a URL.
	return "http://" + net.JoinHostPort(host, port) + healthPath
}

// healthPath must match the route registered in internal/api.
const healthPath = "/api/v1/health"

func defaultDockerHost() string {
	if runtime.GOOS == "windows" {
		return windowsDockerHost
	}
	return unixDockerHost
}

func stringVar(lookup lookupFunc, name, fallback string) string {
	if v, ok := lookup(envPrefix + name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func int64Var(lookup lookupFunc, name string, fallback int64) (int64, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		// The offending value is deliberately omitted from the error.
		return 0, fmt.Errorf("%s%s must be an integer", envPrefix, name)
	}
	return v, nil
}

func floatVar(lookup lookupFunc, name string, fallback float64) (float64, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("%s%s must be a number", envPrefix, name)
	}
	return v, nil
}

func boolVar(lookup lookupFunc, name string, fallback bool) (bool, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s%s must be a boolean such as true or false", envPrefix, name)
	}
	return v, nil
}

// listVar reads a comma-separated list, falling back to a default. Entries are
// trimmed and empties dropped, so "A,,B," yields [A B].
func listVar(lookup lookupFunc, name string, fallback []string) []string {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return append([]string(nil), fallback...)
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func durationVar(lookup lookupFunc, name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s%s must be a duration such as 15s or 1m", envPrefix, name)
	}
	return v, nil
}
