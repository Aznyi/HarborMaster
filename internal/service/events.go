package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The Docker event engine.
//
// It keeps HarborMaster's inventory close to the daemon's reality without
// polling it constantly, on one governing principle:
//
//	AN EVENT IS A HINT, NOT A FACT.
//
// Nothing here writes container state from an event payload. An event says
// "this resource may have changed"; the engine asks the inventory service to go
// and re-read it, so normalization, redaction, checksums, and transaction rules
// stay in exactly one place regardless of what triggered the write. Docker
// inspection and HarborMaster's persistence remain authoritative.
//
// Docker guarantees far less about its event stream than it might appear:
// events emitted while nothing is listening are gone, nothing is replayed
// across a daemon restart, ordering holds only within one connection, and
// duplicates are permitted. Every one of those is why periodic full
// reconciliation stays switched on even when events are flowing perfectly.
//
// GOROUTINE OWNERSHIP. Run is the sole owner. It starts exactly three children
// and returns only once all three have exited, so a caller that waits on Run
// has waited on the whole engine:
//
//	reader    -- connects, reads the Docker stream, pushes onto queue
//	processor -- drains queue, dedupes, persists in batches, classifies
//	worker    -- debounced refreshes, periodic reconciliation, retention
//
// Every child selects on ctx.Done. `queue` is closed by the reader, its only
// writer, so there is one closer and no send-on-closed race. The broadcaster
// owns its own channels and is closed by Run on the way out.

// EventService owns the Docker event stream lifecycle.
type EventService struct {
	runtime   docker.Runtime
	events    *store.DockerEventRepository
	inventory *InventoryService
	logger    *slog.Logger
	cfg       config.Events

	queue       chan domain.DockerEvent
	dedup       *dedupWindow
	scheduler   *refreshScheduler
	broadcaster *eventBroadcaster

	// state guards the connection-status fields. It is never held across a
	// Docker call or a database write, so a status request cannot be blocked by
	// a slow daemon.
	state   sync.RWMutex
	status  engineState
	counter counters

	// shutdownGrace is how long a reconciliation already in flight may run
	// past cancellation, so it can finish its transaction instead of being
	// abandoned mid-write. See GraceContext.
	shutdownGrace time.Duration

	// now and jitter are injectable so timing behaviour is deterministic in
	// tests. Backoff tests must not depend on real elapsed time.
	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// engineState is the mutable connection status.
type engineState struct {
	connection       domain.EventConnectionState
	connectedSince   *time.Time
	lastConnected    *time.Time
	lastDisconnected *time.Time
	lastEvent        *time.Time
	lastReconciled   *time.Time
	currentBackoff   time.Duration
	lastError        string
}

// counters holds the lifetime tallies. Atomics rather than mutex-guarded
// fields: they are written on the event hot path and read by an HTTP handler,
// and neither should wait on the other.
type counters struct {
	received            atomic.Int64
	persisted           atomic.Int64
	deduplicated        atomic.Int64
	dropped             atomic.Int64
	targetedRefreshes   atomic.Int64
	fullReconciliations atomic.Int64
	reconnects          atomic.Int64
	refreshFailures     atomic.Int64
	pruned              atomic.Int64
}

// EventOptions configures an EventService.
type EventOptions struct {
	Runtime   docker.Runtime
	Events    *store.DockerEventRepository
	Inventory *InventoryService
	Logger    *slog.Logger
	Config    config.Events

	// ShutdownGrace bounds how long work already in flight may run past
	// cancellation. Zero selects DefaultShutdownGrace. Wire it from the
	// server's shutdown timeout: the HTTP drain and the background drain share
	// one orchestrator-imposed deadline.
	ShutdownGrace time.Duration

	// Now and Jitter are injectable for deterministic tests. Jitter receives a
	// computed backoff and returns the delay actually waited.
	Now    func() time.Time
	Jitter func(time.Duration) time.Duration
}

// NewEventService builds an EventService.
func NewEventService(opts EventOptions) *EventService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}

	cfg := opts.Config
	if cfg.BufferSize < 1 {
		cfg.BufferSize = config.DefaultEventBufferSize
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = config.DefaultEventBatchSize
	}
	shutdownGrace := opts.ShutdownGrace
	if shutdownGrace <= 0 {
		shutdownGrace = DefaultShutdownGrace
	}

	service := &EventService{
		runtime:   opts.Runtime,
		events:    opts.Events,
		inventory: opts.Inventory,
		logger:    logger,
		cfg:       cfg,
		queue:     make(chan domain.DockerEvent, cfg.BufferSize),
		dedup:     newDedupWindow(cfg.DedupWindow, dedupCapacity(cfg.BufferSize)),
		// The pending-refresh map is capped relative to the queue: past that,
		// tracking resources individually costs more than reconciling.
		scheduler:     newRefreshScheduler(cfg.RefreshDebounce, cfg.BufferSize, now),
		broadcaster:   newEventBroadcaster(cfg.StreamSubscribers, cfg.StreamBuffer),
		shutdownGrace: shutdownGrace,
		now:           now,
		jitter:        jitter,
	}

	service.status.connection = domain.ConnStateDisabled
	if cfg.Enabled {
		service.status.connection = domain.ConnStateConnecting
	}
	return service
}

// dedupCapacity sizes the fingerprint window relative to the queue.
//
// Four queues' worth: enough that a burst filling the queue several times over
// still deduplicates, small enough that the map cannot become a memory problem.
func dedupCapacity(bufferSize int) int {
	capacity := bufferSize * 4
	if capacity < 1024 {
		capacity = 1024
	}
	if capacity > 65536 {
		capacity = 65536
	}
	return capacity
}

// defaultJitter spreads a backoff delay over [50%, 100%] of its nominal value.
//
// Full jitter down to zero would retry instantly against a daemon that is
// down, which is the behaviour the backoff exists to prevent. Halving the floor
// keeps the delay meaningful while still de-synchronising several HarborMasters
// that lost the same daemon at the same moment.
func defaultJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	half := delay / 2
	// math/rand/v2 is correct here, not a weakness. This jitter exists to
	// desynchronise reconnect storms across HarborMaster instances that lost the
	// same daemon at the same moment; it is a scheduling decision, not a secret.
	// crypto/rand would add a syscall per reconnect and buy nothing: an attacker
	// who can predict the jitter learns when a client retries, which is already
	// observable from the connection itself.
	//
	//nolint:gosec // G404: scheduling jitter, not a security decision.
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// Enabled reports whether the event engine is switched on.
func (s *EventService) Enabled() bool { return s.cfg.Enabled }

// Subscribe registers an SSE client. See eventBroadcaster.Subscribe.
func (s *EventService) Subscribe() (*StreamSubscription, error) {
	if !s.cfg.Enabled {
		return nil, ErrEventsDisabled
	}
	return s.broadcaster.Subscribe()
}

// ErrEventsDisabled reports that the event engine is switched off by
// configuration.
var ErrEventsDisabled = errors.New("event engine is disabled")

// Run drives the whole engine until ctx is cancelled.
//
// It returns only once every goroutine it started has exited, so the caller's
// wait group is a complete shutdown barrier. Nothing here is fatal: a Docker
// daemon that is down at startup, or that goes away later, produces reconnect
// attempts and a degraded status, never a process exit.
func (s *EventService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("docker event engine disabled by configuration")
		s.setConnection(domain.ConnStateDisabled)
		return
	}

	// Restore what survived the last run, so status is not blank until the
	// first connection -- and so the FIRST connection after a restart can ask
	// the daemon for what happened while HarborMaster was down, instead of
	// starting blind. See resumePoint for why that window is clamped.
	if state, err := s.events.LoadState(ctx, domain.LocalHostID); err == nil {
		s.state.Lock()
		s.status.lastConnected = state.LastConnectedAt
		s.status.lastDisconnected = state.LastDisconnectedAt
		s.status.lastEvent = state.LastEventAt
		s.status.lastReconciled = state.LastReconciledAt
		s.state.Unlock()
		s.counter.reconnects.Store(state.ReconnectCount)
	}

	s.logger.Info("docker event engine starting",
		slog.Int("queueCapacity", s.cfg.BufferSize),
		slog.Duration("reconcileInterval", s.cfg.ReconcileInterval),
		slog.Duration("debounce", s.cfg.RefreshDebounce))

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		s.readLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		s.processLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		s.workerLoop(ctx)
	}()

	wg.Wait()

	// Last, and only after every producer has stopped: a subscriber blocked on
	// its channel is released so the HTTP server's drain can complete.
	s.broadcaster.Close()
	s.setConnection(domain.ConnStateStopped)
	s.logger.Info("docker event engine stopped")
}

// ---------------------------------------------------------- read loop --

// readLoop maintains the Docker subscription, reconnecting with bounded
// exponential backoff and jitter.
//
// It is the only writer of s.queue and closes it on the way out, which is what
// lets the processor drain cleanly rather than being cancelled mid-batch.
func (s *EventService) readLoop(ctx context.Context) {
	defer close(s.queue)

	backoff := s.cfg.ReconnectInitial
	// resumeFrom lets a reconnect ask the daemon for what it missed. Best
	// effort only: the daemon keeps a bounded in-memory ring, so a long outage
	// returns nothing, which is why every reconnect also reconciles.
	//
	// Seeded from the persisted state, so the first connection after a RESTART
	// recovers the same way a reconnect does. Before this the engine started
	// blind after every restart and relied entirely on the startup
	// reconciliation, which is correct but coarser: it sees the end state and
	// never the transitions.
	resumeFrom := s.resumePoint()

	for {
		if ctx.Err() != nil {
			return
		}

		connectedAt, err := s.readOnce(ctx, resumeFrom)

		if ctx.Err() != nil {
			return
		}

		// A connection that stayed up long enough to be called stable resets
		// the backoff. Without this, a daemon that flaps every few minutes
		// would climb to the maximum delay and stay there, so the FIRST outage
		// after a long healthy period would be the slowest to recover.
		if !connectedAt.IsZero() && s.now().Sub(connectedAt) >= s.stableThreshold() {
			backoff = s.cfg.ReconnectInitial
		}

		s.recordDisconnect(err)

		// Ask for what happened while disconnected, from just after the last
		// event seen. Falling back to the disconnect time when no event was
		// seen keeps the window as narrow as honesty allows.
		resumeFrom = s.resumePoint()

		delay := s.jitter(backoff)
		s.setBackoff(delay)
		s.setConnection(domain.ConnStateReconnecting)
		s.logger.Warn("docker event stream lost; reconnecting",
			slog.Duration("delay", delay),
			slog.String("error", s.lastErrorSummary()))

		// Waiting on a timer inside a select, not a sleep: shutdown during a
		// sixty-second backoff must return immediately, not sixty seconds
		// later. A sleep here would add the full backoff to every shutdown.
		if !s.wait(ctx, delay) {
			return
		}

		backoff = s.nextBackoff(backoff)
		s.counter.reconnects.Add(1)
	}
}

// stableThreshold is how long a connection must hold to count as stable.
//
// Tied to the maximum backoff rather than a constant, so an operator who widens
// the backoff also widens what counts as recovered, and the two cannot drift
// into disagreeing.
func (s *EventService) stableThreshold() time.Duration {
	if s.cfg.ReconnectMax > 0 {
		return s.cfg.ReconnectMax
	}
	return 30 * time.Second
}

// readOnce holds one subscription until it fails or ctx is cancelled.
//
// Returns when the connection was established (zero if it never was) and the
// terminal error.
func (s *EventService) readOnce(ctx context.Context, since time.Time) (time.Time, error) {
	s.setConnection(domain.ConnStateConnecting)

	// Confirm the daemon is reachable BEFORE claiming a connection.
	//
	// The SDK's Events call starts a goroutine and reports a failed HTTP
	// request on the error channel rather than returning it, so subscribing
	// "succeeds" even against a daemon that is not there. Without this check the
	// engine announces a connection, requests a reconciliation, and fails it --
	// once per backoff cycle -- while Docker is simply absent. That is noisy in
	// the log, wrong in the status endpoint, and wasted work against a socket
	// that is not answering.
	if _, err := s.runtime.Ping(ctx); err != nil {
		return time.Time{}, err
	}

	// Its own cancellable context so the SDK's reader goroutine and the
	// adapter's converter both stop when this subscription ends, rather than
	// leaking until process exit.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	subscription, err := s.runtime.StreamEvents(streamCtx, since)
	if err != nil {
		// Docker was unavailable at connect time. Expected at startup on a host
		// where the daemon comes up after HarborMaster.
		return time.Time{}, err
	}

	// The SDK resolves the /events request before returning, so a failure is
	// usually already waiting here. A non-blocking peek costs nothing and can
	// only be right: an error present at this point means the stream never
	// opened. It can miss one that lands a moment later, which the loop below
	// then handles as an immediate disconnect.
	select {
	case streamErr := <-subscription.Errors:
		if streamErr == nil {
			streamErr = errors.New("docker event stream closed")
		}
		return time.Time{}, streamErr
	default:
	}

	connectedAt := s.now()
	s.recordConnect(connectedAt)

	// Every (re)connection reconciles. Anything that happened while
	// disconnected produced no event HarborMaster saw, and the daemon's replay
	// is bounded and unreliable, so a full sweep is the only way to be sure the
	// inventory is right.
	s.scheduler.requestFull(domain.TriggerReconcile)

	for {
		select {
		case <-ctx.Done():
			return connectedAt, ctx.Err()

		case err := <-subscription.Errors:
			// The SDK sends exactly one terminal error, io.EOF included.
			if err == nil {
				err = errors.New("docker event stream closed")
			}
			return connectedAt, err

		case event, ok := <-subscription.Events:
			if !ok {
				return connectedAt, errors.New("docker event stream closed")
			}
			s.enqueue(event)
		}
	}
}

// enqueue puts an event on the processing queue without ever blocking.
//
// A blocking send here would stall the Docker reader, which back-pressures the
// daemon's own event dispatch. Instead the event is dropped and a full
// reconciliation requested: a dropped event may have been the only notice of a
// change, so nothing narrower than a sweep is honest about the gap.
func (s *EventService) enqueue(event domain.DockerEvent) {
	s.counter.received.Add(1)

	observed := s.now()
	event.ObservedAt = observed
	event.CreatedAt = observed
	if event.DockerTime.IsZero() {
		// A daemon that sent no timestamp. Observation time is the closest
		// honest answer, and it keeps the row out of 1970 in every ordering.
		event.DockerTime = observed
	}
	event.ConnectionState = s.connectionState()

	select {
	case s.queue <- event:
		s.noteEventTime(observed)
	default:
		s.counter.dropped.Add(1)
		s.scheduler.markOverflow()
		// Deliberately not per-event: an overflow means events are arriving
		// faster than they can be stored, and logging each drop would make the
		// log the next bottleneck. The counter carries the volume; this line
		// carries the fact.
		s.logger.Warn("docker event queue full; event dropped and a full reconciliation requested",
			slog.Int("queueCapacity", s.cfg.BufferSize),
			slog.Int64("droppedTotal", s.counter.dropped.Load()))
	}
}

// wait sleeps for delay, returning false if ctx was cancelled first.
func (s *EventService) wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextBackoff advances the delay, capped at the configured maximum.
func (s *EventService) nextBackoff(current time.Duration) time.Duration {
	factor := s.cfg.ReconnectFactor
	if factor < 1 {
		factor = 1
	}
	next := time.Duration(float64(current) * factor)
	if next <= current {
		// Guards a factor of exactly 1 and any rounding that would stall
		// progress: the delay must never shrink.
		next = current
	}
	if s.cfg.ReconnectMax > 0 && next > s.cfg.ReconnectMax {
		next = s.cfg.ReconnectMax
	}
	return next
}

// ------------------------------------------------------- process loop --

// processLoop drains the queue, deduplicating, persisting in batches, and
// classifying what each event asks of the inventory.
//
// It exits when the queue is closed (the reader has stopped) and drained, so
// events already read are not thrown away by a shutdown.
func (s *EventService) processLoop(ctx context.Context) {
	batch := make([]domain.DockerEvent, 0, s.cfg.BatchSize)

	// One timer for the whole loop, re-armed per partial batch. A timer per
	// event would be one allocation and one runtime timer per event.
	flush := time.NewTimer(s.cfg.BatchFlush)
	defer flush.Stop()
	if !flush.Stop() {
		<-flush.C
	}
	timerArmed := false

	armTimer := func() {
		if !timerArmed {
			flush.Reset(s.cfg.BatchFlush)
			timerArmed = true
		}
	}
	disarmTimer := func() {
		if timerArmed {
			if !flush.Stop() {
				select {
				case <-flush.C:
				default:
				}
			}
			timerArmed = false
		}
	}

	for {
		select {
		case event, ok := <-s.queue:
			if !ok {
				// The reader has stopped and the queue is drained. Flush what
				// is held using a context detached from cancellation, so events
				// already read are not lost to shutdown -- bounded, so a wedged
				// database cannot hold the process open.
				disarmTimer()
				s.flush(context.WithoutCancel(ctx), batch)
				return
			}

			if !s.dedup.admit(event.Fingerprint, event.ObservedAt) {
				s.counter.deduplicated.Add(1)
				// Not persisted and no refresh scheduled. The counter is the
				// record; storing a row per duplicate would fill the history
				// with noise the unique constraint would reject anyway.
				continue
			}

			batch = append(batch, event)
			if len(batch) >= s.cfg.BatchSize {
				disarmTimer()
				s.flush(ctx, batch)
				batch = batch[:0]
				continue
			}
			armTimer()

		case <-flush.C:
			timerArmed = false
			if len(batch) > 0 {
				s.flush(ctx, batch)
				batch = batch[:0]
			}

		case <-ctx.Done():
			// Drain whatever the reader already produced before leaving, then
			// let the queue-closed branch above do the final flush on the next
			// pass. Returning here would discard the batch in hand.
			disarmTimer()
			s.flush(context.WithoutCancel(ctx), batch)
			return
		}
	}
}

// flushTimeout bounds a batch write during shutdown, so a wedged database
// cannot hold the process open indefinitely.
const flushTimeout = 5 * time.Second

// flush persists a batch and schedules whatever refreshes it implies.
//
// Classification happens BEFORE persistence so the recorded row carries the
// decision, and scheduling happens AFTER, so nothing is refreshed on account of
// an event that failed to store.
func (s *EventService) flush(ctx context.Context, batch []domain.DockerEvent) {
	if len(batch) == 0 {
		return
	}

	classifications := make([]Classification, len(batch))
	for i := range batch {
		classification := ClassifyEvent(batch[i])
		classifications[i] = classification

		batch[i].Result = classification.Result
		batch[i].RefreshRequested = classification.Request
		if classification.Result == domain.ResultWarning {
			batch[i].Error = classification.Reason
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()

	stored, duplicates, err := s.events.Append(writeCtx, batch)
	if err != nil {
		// Persistence failed for the whole batch. The events are lost, so the
		// same reasoning as a queue overflow applies: reconcile rather than
		// pretend nothing changed.
		s.counter.refreshFailures.Add(1)
		s.setLastError("event history could not be written")
		s.scheduler.requestFull(domain.TriggerReconcile)
		s.logger.Error("could not persist docker events; requesting reconciliation",
			slog.Int("batch", len(batch)),
			slog.String("error", err.Error()))
		return
	}

	if duplicates > 0 {
		s.counter.deduplicated.Add(int64(duplicates))
	}
	s.counter.persisted.Add(int64(len(stored)))

	// Only stored events are broadcast and acted on: a duplicate rejected by
	// the unique constraint has already had its effect.
	for _, event := range stored {
		if dropped := s.broadcaster.Publish(event); dropped > 0 {
			s.logger.Debug("sse subscribers dropped an event",
				slog.Int("subscribers", dropped),
				slog.Int64("sequence", event.Sequence))
		}

		classification := classificationFor(batch, classifications, event.Fingerprint)
		if classification.Request == domain.RefreshNone {
			continue
		}
		if classification.Request == domain.RefreshFull {
			s.logger.Info("event requested a full reconciliation",
				slog.String("action", string(event.Action)),
				slog.String("reason", classification.Reason))
			s.scheduler.requestFull(domain.TriggerReconcile)
			continue
		}
		if overflowed := s.scheduler.request(classification.Request, classification.Target); overflowed {
			s.logger.Warn("pending refresh queue full; escalating to a full reconciliation",
				slog.Int("capacity", s.cfg.BufferSize))
		}
	}
}

// classificationFor recovers a stored event's classification.
//
// Matched by fingerprint rather than by index because Append returns only the
// events it actually stored, so the two slices are not parallel.
func classificationFor(batch []domain.DockerEvent, classifications []Classification, fingerprint string) Classification {
	for i := range batch {
		if batch[i].Fingerprint == fingerprint {
			return classifications[i]
		}
	}
	return Classification{Request: domain.RefreshNone}
}

// -------------------------------------------------------- worker loop --

// workerLoop performs debounced refreshes, periodic reconciliation, and
// retention.
//
// One goroutine for all three, driven by one select. They are combined because
// they must not overlap: a reconciliation while a targeted refresh is mid-write
// buys nothing, and pruning while either runs would contend for the single
// SQLite writer.
func (s *EventService) workerLoop(ctx context.Context) {
	reconcile := time.NewTicker(s.cfg.ReconcileInterval)
	defer reconcile.Stop()

	prune := time.NewTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	// One timer, re-armed as the debounce queue changes. Not one per pending
	// resource: a thousand containers must not mean a thousand runtime timers.
	debounce := time.NewTimer(time.Hour)
	defer debounce.Stop()
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceArmed := false

	for {
		full, trigger, targeted, wait := s.scheduler.due(s.now())

		switch {
		case full:
			s.runReconciliation(ctx, trigger)
			continue
		case len(targeted) > 0:
			s.runTargeted(ctx, targeted)
			continue
		}

		if wait > 0 && !debounceArmed {
			debounce.Reset(wait)
			debounceArmed = true
		}

		select {
		case <-ctx.Done():
			return

		case <-s.scheduler.wakeup():
			// New work arrived. Re-arm from scratch on the next pass: the new
			// item may be due sooner than whatever the timer was set for.
			if debounceArmed {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounceArmed = false
			}

		case <-debounce.C:
			debounceArmed = false

		case <-reconcile.C:
			// The single periodic full sweep. The inventory service's own
			// ticker is suppressed while the engine runs, so there is exactly
			// one of these timers in the process.
			s.scheduler.requestFull(domain.TriggerReconcile)

		case <-prune.C:
			s.runRetention(ctx)
		}
	}
}

// runTargeted performs a batch of debounced refreshes.
//
// Sequential rather than concurrent: SQLite tolerates one writer, and each of
// these is an inspect followed by a write. Running them in parallel would queue
// at the database anyway while multiplying the load on the Docker socket.
func (s *EventService) runTargeted(ctx context.Context, keys []refreshKey) {
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		refreshCtx, cancel := TargetedContext(ctx)
		err := s.performTargeted(refreshCtx, key)
		cancel()

		if err == nil {
			s.counter.targetedRefreshes.Add(1)
			continue
		}

		s.counter.refreshFailures.Add(1)

		// A targeted refresh that cannot run is escalated rather than retried.
		// Retrying against a daemon that just failed would spin; a
		// reconciliation is bounded, covers this resource, and repairs anything
		// else that was missed for the same reason.
		summary := docker.SanitizeError(err)
		if errors.Is(err, ErrTargetedUnavailable) {
			summary = "no inventory generation exists yet"
		}
		s.setLastError(summary)
		s.logger.Warn("targeted refresh failed; requesting reconciliation",
			slog.String("kind", string(key.kind)),
			slog.String("target", domain.ShortenID(key.target)),
			slog.String("error", summary))
		s.scheduler.requestFull(domain.TriggerReconcile)
		return
	}
}

func (s *EventService) performTargeted(ctx context.Context, key refreshKey) error {
	switch key.kind {
	case domain.RefreshContainer:
		return s.inventory.RefreshContainer(ctx, key.target)
	case domain.RefreshContainerAbsent:
		return s.inventory.MarkContainerAbsent(ctx, key.target)
	case domain.RefreshImage:
		return s.inventory.RefreshImage(ctx, key.target)
	case domain.RefreshImageCatalog:
		// No single image to inspect: a delete names something gone and a prune
		// names nothing. The container sweep a reconciliation performs is what
		// resolves which images are still referenced, so this defers to it
		// rather than inventing a narrower pass that could not be correct.
		return ErrTargetedUnavailable
	case domain.RefreshNetworks:
		return s.inventory.RefreshNetworks(ctx)
	case domain.RefreshVolumes:
		return s.inventory.RefreshVolumes(ctx)
	default:
		return errors.New("unknown refresh kind")
	}
}

// runReconciliation performs one full inventory sweep through the Phase 2
// pipeline.
func (s *EventService) runReconciliation(ctx context.Context, trigger domain.RefreshTrigger) {
	if trigger == "" {
		trigger = domain.TriggerReconcile
	}
	s.counter.fullReconciliations.Add(1)

	// Survives cancellation for the shutdown grace period and no longer. A
	// sweep of a large host must not be aborted mid-transaction by a signal --
	// and must not hold the process open past the deadline the orchestrator is
	// counting down either. See GraceContext for why both halves matter.
	refreshCtx, cancel := GraceContext(ctx, s.shutdownGrace, maxAsyncRefresh)
	defer cancel()

	err := s.inventory.Reconcile(refreshCtx, trigger)
	switch {
	case err == nil:
		at := s.now()
		s.state.Lock()
		s.status.lastReconciled = &at
		s.state.Unlock()
		s.scheduler.clearOverflow()
		s.persistState(ctx)
		s.logger.Info("event-driven reconciliation complete",
			slog.String("trigger", string(trigger)))

	case errors.Is(err, ErrRefreshInProgress):
		// A sweep is already running and will cover the same ground. Treating
		// this as satisfied is correct; retrying would queue a redundant pass.
		s.scheduler.clearOverflow()
		s.logger.Debug("reconciliation skipped; a refresh is already running")

	case errors.Is(err, ErrInventoryDisabled):
		s.logger.Debug("reconciliation skipped; the inventory engine is disabled")

	default:
		s.counter.refreshFailures.Add(1)
		s.setLastError(docker.SanitizeError(err))
		s.logger.Warn("reconciliation failed",
			slog.String("trigger", string(trigger)),
			slog.String("error", docker.SanitizeError(err)))
	}
}

// runRetention prunes event history.
func (s *EventService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 && s.cfg.RetentionCount <= 0 {
		return
	}

	removed, err := s.events.Prune(ctx, store.PruneOptions{
		MaxAge:   s.cfg.RetentionAge,
		MaxCount: s.cfg.RetentionCount,
		Now:      s.now(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("event retention pass failed", slog.String("error", err.Error()))
		return
	}
	if removed == 0 {
		return
	}

	s.counter.pruned.Add(removed)
	s.logger.Info("pruned docker event history",
		slog.Int64("removed", removed),
		slog.Duration("maxAge", s.cfg.RetentionAge),
		slog.Int64("maxCount", s.cfg.RetentionCount))
}

// PruneNow runs one retention pass immediately.
//
// Exported for tests and for a future operator-facing tool. Deliberately NOT
// reachable from the HTTP API: this phase exposes no destructive endpoint, and
// an unauthenticated one that deletes history would be exactly the wrong thing
// to add first.
func (s *EventService) PruneNow(ctx context.Context) (int64, error) {
	return s.events.Prune(ctx, store.PruneOptions{
		MaxAge:   s.cfg.RetentionAge,
		MaxCount: s.cfg.RetentionCount,
		Now:      s.now(),
	})
}

// ------------------------------------------------------------- status --

// Status assembles the event-engine status payload.
func (s *EventService) Status(ctx context.Context) domain.EventEngineStatus {
	s.state.RLock()
	state := s.status
	s.state.RUnlock()

	pending, _, overflowed := s.scheduler.snapshot()

	status := domain.EventEngineStatus{
		Enabled:          s.cfg.Enabled,
		State:            state.connection,
		ConnectedSince:   state.connectedSince,
		LastConnectedAt:  state.lastConnected,
		LastDisconnected: state.lastDisconnected,
		LastEventAt:      state.lastEvent,
		LastReconcileAt:  state.lastReconciled,
		CurrentBackoffMS: state.currentBackoff.Milliseconds(),
		QueueDepth:       len(s.queue),
		QueueCapacity:    cap(s.queue),
		PendingRefreshes: pending,
		OverflowPending:  overflowed,
		Subscribers:      s.broadcaster.Subscribers(),
		SubscriberLimit:  s.broadcaster.Limit(),
		LastError:        state.lastError,
		Counters: domain.EventEngineCounters{
			Received:            s.counter.received.Load(),
			Persisted:           s.counter.persisted.Load(),
			Deduplicated:        s.counter.deduplicated.Load(),
			Dropped:             s.counter.dropped.Load(),
			TargetedRefreshes:   s.counter.targetedRefreshes.Load(),
			FullReconciliations: s.counter.fullReconciliations.Load(),
			Reconnects:          s.counter.reconnects.Load(),
			RefreshFailures:     s.counter.refreshFailures.Load(),
			Pruned:              s.counter.pruned.Load(),
		},
		Retention: domain.EventRetentionPolicy{
			MaxAgeSeconds:   int64(s.cfg.RetentionAge.Seconds()),
			MaxCount:        s.cfg.RetentionCount,
			IntervalSeconds: int64(s.cfg.PruneInterval.Seconds()),
		},
	}

	// A count query per status call is acceptable: the endpoint is polled every
	// few seconds at most, and COUNT(*) on an indexed table is cheap.
	if count, err := s.events.Count(ctx); err == nil {
		status.StoredEvents = count
	}

	return status
}

// Degraded reports whether the event engine's state should degrade application
// health.
//
// Two conditions, and only two:
//
//   - The stream is meant to be connected and is not. Inventory is still
//     served and periodic reconciliation still runs, so this is degraded
//     rather than unhealthy.
//   - A queue overflow forced a reconciliation that has not completed. Until it
//     does, the inventory may be missing a change.
//
// A DISABLED engine is not degraded. Running on periodic reconciliation alone
// is a supported configuration, and reporting it as degraded would make the
// container health check cry wolf forever.
func (s *EventService) Degraded() (bool, string) {
	if !s.cfg.Enabled {
		return false, ""
	}

	if _, _, overflowed := s.scheduler.snapshot(); overflowed {
		return true, "event queue overflowed; reconciliation pending"
	}

	switch s.connectionState() {
	case domain.ConnStateConnected:
		return false, ""
	case domain.ConnStateStopped:
		return false, ""
	default:
		return true, "docker event stream disconnected; relying on periodic reconciliation"
	}
}

// ReplayLimit reports the configured SSE replay cap.
func (s *EventService) ReplayLimit() int { return s.cfg.StreamReplay }

// HeartbeatInterval reports the configured SSE heartbeat period.
func (s *EventService) HeartbeatInterval() time.Duration { return s.cfg.StreamHeartbeat }

// ReconnectHint reports the delay an SSE client should wait before reconnecting.
func (s *EventService) ReconnectHint() time.Duration { return s.cfg.ReconnectInitial }

// -------------------------------------------------------- state helpers --

func (s *EventService) setConnection(state domain.EventConnectionState) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.connection = state
}

func (s *EventService) connectionState() domain.EventConnectionState {
	s.state.RLock()
	defer s.state.RUnlock()
	return s.status.connection
}

func (s *EventService) setBackoff(delay time.Duration) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.currentBackoff = delay
}

// setLastError records a SANITISED failure summary.
//
// Callers must pass something already safe to expose: this value reaches the
// status endpoint, which is unauthenticated. A raw Docker error can name the
// socket path and daemon internals.
func (s *EventService) setLastError(summary string) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.lastError = summary
}

func (s *EventService) lastErrorSummary() string {
	s.state.RLock()
	defer s.state.RUnlock()
	return s.status.lastError
}

func (s *EventService) recordConnect(at time.Time) {
	s.state.Lock()
	s.status.connection = domain.ConnStateConnected
	s.status.connectedSince = &at
	s.status.lastConnected = &at
	s.status.currentBackoff = 0
	s.status.lastError = ""
	s.state.Unlock()

	s.logger.Info("docker event stream connected")
	s.persistState(context.Background())
}

func (s *EventService) recordDisconnect(err error) {
	at := s.now()

	s.state.Lock()
	s.status.connection = domain.ConnStateReconnecting
	s.status.connectedSince = nil
	s.status.lastDisconnected = &at
	if err != nil {
		// Sanitised at the boundary: the raw error can carry a socket path.
		s.status.lastError = docker.SanitizeError(err)
	}
	s.state.Unlock()

	s.persistState(context.Background())
}

func (s *EventService) noteEventTime(at time.Time) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.lastEvent = &at
}

// maxResumeLookback bounds how far back a reconnect asks the daemon to replay.
//
// The window is an UNBOUNDED input if it is not clamped. A HarborMaster that
// was stopped for a month would, on restart, ask the daemon for a month of
// events; the daemon walks its ring to satisfy that, and whatever it returns
// arrives as a burst against a queue sized for steady state -- so the recovery
// path becomes the load spike. An hour is longer than any realistic restart
// and far shorter than any outage a replay could actually cover, and every
// (re)connection reconciles regardless, so the clamp costs no correctness.
const maxResumeLookback = time.Hour

// resumePoint chooses where a reconnect asks the daemon to resume from.
//
// Clamped to maxResumeLookback: a resume point far in the past is a request
// for an unbounded replay, and this is a read against a privileged socket.
func (s *EventService) resumePoint() time.Time {
	s.state.RLock()
	defer s.state.RUnlock()

	var from time.Time
	switch {
	case s.status.lastEvent != nil:
		// One nanosecond past the last event seen, so the resume neither
		// replays it nor skips one that shared its instant.
		from = s.status.lastEvent.Add(time.Nanosecond)
	case s.status.lastDisconnected != nil:
		from = *s.status.lastDisconnected
	default:
		return time.Time{}
	}

	if earliest := s.now().Add(-maxResumeLookback); from.Before(earliest) {
		return earliest
	}
	return from
}

// persistState writes the small slice of status worth surviving a restart.
//
// Bounded and best effort: failing to record a timestamp must never affect the
// engine, so the error is logged at debug and dropped.
func (s *EventService) persistState(ctx context.Context) {
	s.state.RLock()
	state := store.EngineState{
		LastConnectedAt:    s.status.lastConnected,
		LastDisconnectedAt: s.status.lastDisconnected,
		LastEventAt:        s.status.lastEvent,
		LastReconciledAt:   s.status.lastReconciled,
		LastError:          s.status.lastError,
	}
	s.state.RUnlock()
	state.ReconnectCount = s.counter.reconnects.Load()

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	if err := s.events.SaveState(writeCtx, domain.LocalHostID, state); err != nil {
		s.logger.Debug("could not persist event engine state", slog.String("error", err.Error()))
	}
}
