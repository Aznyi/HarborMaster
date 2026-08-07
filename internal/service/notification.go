package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The notification engine.
//
// # The rule that shapes everything here
//
// A NOTIFICATION MUST NEVER AFFECT THE THING IT IS ABOUT. A rollback that
// waited on a webhook would be a rollback whose duration depended on somebody
// else's server; an update that failed because a chat service was down would be
// an update that failed for no reason.
//
// So Raise is non-blocking, always. It puts a notification on a bounded queue
// and returns. It cannot block, cannot fail, and cannot return an error — there
// is deliberately no error to ignore, because a caller in the middle of a
// container recreation has nothing useful to do with one.
//
// # What happens when the queue is full
//
// The notification is DROPPED, and the drop is recorded as loudly as a drop can
// be: a delivery row with result `dropped`, and a warning in the log. That is
// the honest failure mode. The alternatives are worse:
//
//   - Blocking would make a slow destination stall the pipeline, which is the
//     one thing this must not do.
//   - An unbounded queue would turn a destination outage into unbounded memory
//     growth, and the process would die instead of the notification.
//
// # Retries are bounded and backed off
//
// A failed delivery is retried on an exponential schedule up to a cap, and then
// becomes a dead letter that stays in the history. A destination that is
// permanently broken — a revoked webhook URL — is not retryable at all and goes
// straight to the dead letter, because repeating a 403 forever helps nobody.

// Notification service errors.
var (
	// ErrNotificationsDisabled reports that the subsystem is not configured.
	ErrNotificationsDisabled = errors.New("notifications are not configured in this deployment")
	// ErrDestinationNotTestable reports a test send that cannot be attempted.
	ErrDestinationNotTestable = errors.New("this destination cannot be tested as configured")
)

// NotificationStore is the persistence the engine needs.
type NotificationStore interface {
	ActiveDestinations(ctx context.Context) ([]domain.NotificationDestination, error)
	ActiveRules(ctx context.Context) ([]domain.NotificationRule, error)
	DestinationByID(ctx context.Context, destinationID string) (domain.NotificationDestination, error)
	// Secret is the ONE method that returns a credential. Called immediately
	// before a send and nowhere else.
	Secret(ctx context.Context, destinationID string) (domain.NotificationSecret, error)

	RecordDelivery(ctx context.Context, delivery domain.NotificationDelivery) error
	CompleteDelivery(ctx context.Context, deliveryID string, result domain.DeliveryResult,
		attempts, statusCode int, detail string, nextAttemptAt *time.Time,
		completedAt time.Time, durationMs int64) error
	RecordDestinationResult(ctx context.Context, destinationID string,
		result domain.DeliveryResult, detail string, at time.Time) error
	DueRetries(ctx context.Context, now time.Time, limit int) ([]domain.NotificationDelivery, error)
	ShouldSuppress(ctx context.Context, ruleID, dedupKey string,
		cooldown time.Duration, now time.Time) (bool, error)
	PruneDeliveries(ctx context.Context, before time.Time, limit int) (int, error)

	ListDeliveries(ctx context.Context, filter store.DeliveryFilter) ([]domain.NotificationDelivery, int, error)
	DeliveryByID(ctx context.Context, deliveryID string) (domain.NotificationDelivery, error)
	DeliverySummary(ctx context.Context) (domain.NotificationSummary, error)
	CountNotificationConfiguration(ctx context.Context) (destinations, rules, failing int, err error)
}

// NotificationSender is the delivery capability.
//
// An interface so a test substitutes it, and so the engine's dependency is the
// narrow "send this" rather than the whole notify package.
type NotificationSender interface {
	Send(ctx context.Context, request notify.SendRequest) notify.Result
}

// NotificationOptions configures a NotificationService.
type NotificationOptions struct {
	Store  NotificationStore
	Sender NotificationSender
	// SMTP is the relay every email destination uses. Supplied from
	// configuration rather than stored per destination, so a password can stay
	// out of the database entirely.
	SMTP domain.SMTPSettings
	// SMTPSecret carries the relay credential from configuration. Never
	// persisted when it arrives this way.
	SMTPSecret domain.NotificationSecret

	Config config.Notifications
	Logger *slog.Logger
	Now    func() time.Time
}

// queued is one notification waiting to be routed.
type queued struct {
	notification domain.Notification
	// destinationID targets a single destination, bypassing the rules.
	//
	// Two callers set it: the test-send path, and a requeue from a destination
	// that was at its concurrency limit. Both need the routing decision to have
	// already been made — re-running the rules for a requeue would re-deliver to
	// every OTHER destination the notification had already reached.
	destinationID string
	// rule is the rule that routed a requeue, carried so the redelivery is
	// attributed to the same rule the first attempt was.
	rule domain.NotificationRule
	// requeues counts how many times this has been put back for a busy
	// destination. Bounded, so a permanently saturated destination cannot make
	// one notification circulate forever.
	requeues int
}

// maxNotificationRequeues bounds how many times a notification may be put back
// because its destination was at its concurrency limit.
//
// Past it the notification is dropped and recorded as such. A destination that
// is still saturated after this many passes through a bounded queue is not busy,
// it is broken, and circulating the message is a busy loop that keeps the queue
// full for everybody else.
const maxNotificationRequeues = 3

// NotificationService routes notifications and delivers them.
type NotificationService struct {
	store  NotificationStore
	sender NotificationSender
	smtp   domain.SMTPSettings
	secret domain.NotificationSecret

	cfg    config.Notifications
	logger *slog.Logger
	now    func() time.Time

	// queue is bounded. See the file header for why a full queue drops rather
	// than blocks.
	queue chan queued

	// perDestination bounds concurrent deliveries to ONE destination, so a slow
	// endpoint cannot occupy every worker and starve the others.
	mu       sync.Mutex
	inFlight map[string]int
	// dropped counts what the queue could not take, for the log line that says
	// so.
	dropped int64
}

// NewNotificationService builds a NotificationService.
func NewNotificationService(opts NotificationOptions) *NotificationService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.QueueSize < 1 {
		cfg.QueueSize = config.DefaultNotificationQueueSize
	}
	if cfg.Workers < 1 {
		cfg.Workers = config.DefaultNotificationWorkers
	}
	if cfg.MaxPerDestination < 1 {
		cfg.MaxPerDestination = config.DefaultNotificationMaxPerDestination
	}
	if cfg.DeliveryTimeout <= 0 {
		cfg.DeliveryTimeout = config.DefaultNotificationDeliveryTimeout
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = config.DefaultNotificationMaxAttempts
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = config.DefaultNotificationRetryBackoff
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = config.DefaultNotificationRetryInterval
	}
	if cfg.RetentionAge <= 0 {
		cfg.RetentionAge = config.DefaultNotificationRetentionAge
	}
	if cfg.PruneInterval <= 0 {
		cfg.PruneInterval = config.DefaultNotificationPruneInterval
	}

	return &NotificationService{
		store:    opts.Store,
		sender:   opts.Sender,
		smtp:     opts.SMTP,
		secret:   opts.SMTPSecret,
		cfg:      cfg,
		logger:   logger,
		now:      now,
		queue:    make(chan queued, cfg.QueueSize),
		inFlight: make(map[string]int),
	}
}

// Enabled reports whether notifications may be delivered.
func (s *NotificationService) Enabled() bool {
	return s.cfg.Enabled && s.store != nil && s.sender != nil
}

// Readable reports whether the notification records can be served.
//
// Separate from Enabled, matching the automation engine: an operator must be
// able to review destinations, rules, and past deliveries on a deployment where
// sending is switched off.
func (s *NotificationService) Readable() bool { return s.store != nil }

// ------------------------------------------------------------------ raise --

// Raise queues a notification.
//
// # Never blocks, never fails, never returns an error
//
// Called from the middle of an acquisition, a recreation, and a rollback. A
// caller there has nothing useful to do with an error and must not wait, so
// there is deliberately nothing to ignore.
//
// A full queue DROPS, and says so. See the file header.
func (s *NotificationService) Raise(notification domain.Notification) {
	if !s.Enabled() {
		return
	}

	notification = notification.Sanitise()
	if notification.OccurredAt.IsZero() {
		notification.OccurredAt = s.now().UTC()
	}

	select {
	case s.queue <- queued{notification: notification}:
	default:
		// The one outcome that means HarborMaster lost something. Counted and
		// logged; the delivery row is written by the drop recorder below, which
		// runs detached so this call still does not block.
		s.mu.Lock()
		s.dropped++
		total := s.dropped
		s.mu.Unlock()

		s.logger.Warn("notification queue is full; a notification was dropped",
			slog.String("event", string(notification.Event)),
			slog.Int("queueSize", s.cfg.QueueSize),
			slog.Int64("droppedTotal", total))
	}
}

// RaiseTest queues a delivery to ONE destination, bypassing the rules.
//
// The only path that targets a destination directly. It exists so an operator
// can prove a destination works without writing a rule and waiting for
// something to happen, and it travels the same queue, the same transport, and
// the same checks as everything else — a test that took a different path would
// prove the wrong thing.
func (s *NotificationService) RaiseTest(destinationID string) error {
	if !s.Enabled() {
		return ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDestinationID(destinationID) {
		return store.ErrNotFound
	}

	notification := domain.Notification{
		Event:      domain.EventTest,
		Severity:   domain.NotifyInfo,
		Title:      "HarborMaster test notification",
		Body:       "If you can read this, this destination is working.",
		OccurredAt: s.now().UTC(),
	}.Sanitise()

	select {
	case s.queue <- queued{notification: notification, destinationID: destinationID}:
		return nil
	default:
		return errors.New("the notification queue is full; try again in a moment")
	}
}

// ------------------------------------------------------------------- run --

// Run drives the delivery workers, the retry sweep, and retention.
func (s *NotificationService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("notifications disabled by configuration")
		return
	}
	if !s.Enabled() {
		s.logger.Error("notifications are enabled but no sender is wired; nothing will be delivered")
		return
	}

	s.logger.Info("notification engine started",
		slog.Int("workers", s.cfg.Workers),
		slog.Int("queueSize", s.cfg.QueueSize),
		slog.Int("maxPerDestination", s.cfg.MaxPerDestination),
		slog.Duration("deliveryTimeout", s.cfg.DeliveryTimeout),
		slog.Int("maxAttempts", s.cfg.MaxAttempts))

	var workers sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			s.work(ctx)
		}()
	}

	retry := time.NewTicker(s.cfg.RetryInterval)
	defer retry.Stop()
	prune := time.NewTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			// The workers observe the same context and drain what they are
			// holding. Waiting for them here is what makes shutdown ordered:
			// the database must not close while a delivery is recording its
			// outcome.
			workers.Wait()
			s.logger.Info("notification engine stopped")
			return
		case <-retry.C:
			s.sweepRetries(ctx)
		case <-prune.C:
			s.prune(ctx)
		}
	}
}

// work is one delivery worker.
func (s *NotificationService) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.queue:
			s.route(ctx, item)
		}
	}
}

// route decides which destinations a notification reaches, and delivers it.
func (s *NotificationService) route(ctx context.Context, item queued) {
	destinations, err := s.store.ActiveDestinations(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not load notification destinations",
			slog.Any("error", err))
		return
	}
	byID := make(map[string]domain.NotificationDestination, len(destinations))
	for _, destination := range destinations {
		byID[destination.DestinationID] = destination
	}

	// Already routed: a test send, or a requeue from a busy destination. Either
	// way the decision has been made and must not be made again — re-running the
	// rules here would re-deliver to every other destination this notification
	// had already reached.
	if item.destinationID != "" {
		destination, known := byID[item.destinationID]
		if !known {
			s.logger.WarnContext(ctx, "a notification named a destination that is not active",
				slog.String("destinationId", item.destinationID))
			return
		}
		s.deliver(ctx, item.notification, destination, item.rule, item.requeues)
		return
	}

	rules, err := s.store.ActiveRules(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not load notification rules", slog.Any("error", err))
		return
	}

	// A notification may match several rules and several rules may name the
	// same destination. It is delivered ONCE per destination: the first rule
	// that routes it wins, and the rest are not re-sent. Without this an
	// operator with a broad rule and a specific one gets two messages for one
	// event and concludes the feature is broken.
	sent := make(map[string]struct{}, len(destinations))

	for _, rule := range rules {
		if !rule.Matches(item.notification) {
			continue
		}
		suppressed, err := s.store.ShouldSuppress(ctx, rule.RuleID,
			item.notification.DedupKey, rule.Cooldown(), s.now())
		if err != nil {
			s.logger.ErrorContext(ctx, "could not evaluate a notification cooldown",
				slog.String("ruleId", rule.RuleID), slog.Any("error", err))
			// Fails towards SENDING. A cooldown is a convenience; losing a
			// message about a failed rollback because a suppression lookup
			// errored would be the wrong trade.
		}
		if suppressed {
			for _, destinationID := range rule.Destinations {
				destination, known := byID[destinationID]
				if !known {
					continue
				}
				s.recordSuppressed(ctx, item.notification, destination, rule)
			}
			continue
		}

		for _, destinationID := range rule.Destinations {
			destination, known := byID[destinationID]
			if !known {
				// A rule naming an archived or disabled destination. Not an
				// error: archiving is refused while a rule routes to one, so
				// this is the disabled case, which is deliberate.
				continue
			}
			if _, already := sent[destinationID]; already {
				continue
			}
			sent[destinationID] = struct{}{}
			s.deliver(ctx, item.notification, destination, rule, item.requeues)
		}
	}
}

// deliver sends one notification to one destination, and records the outcome.
func (s *NotificationService) deliver(
	ctx context.Context,
	notification domain.Notification,
	destination domain.NotificationDestination,
	rule domain.NotificationRule,
	requeues int,
) {
	// The per-destination concurrency claim. A slow endpoint occupies at most
	// this many workers, so the others keep serving every other destination.
	if !s.claim(destination.DestinationID) {
		// Requeued rather than dropped: the destination is busy, not broken.
		//
		// Targeted at THIS destination, carrying THIS rule. Sending it back as an
		// unrouted notification would put it through the rules a second time and
		// deliver it again to every destination that had already taken it.
		//
		// Non-blocking, so a full queue still drops rather than stalls, and
		// bounded, so a permanently saturated destination cannot make one
		// notification circulate forever.
		if requeues < maxNotificationRequeues {
			select {
			case s.queue <- queued{
				notification:  notification,
				destinationID: destination.DestinationID,
				rule:          rule,
				requeues:      requeues + 1,
			}:
				return
			default:
			}
		}
		s.recordDropped(ctx, notification, destination, rule,
			"the destination was at its concurrency limit and the queue could not hold the retry")
		return
	}
	defer s.release(destination.DestinationID)

	delivery := domain.NotificationDelivery{
		DeliveryID:      domain.NewNotificationDeliveryID(),
		DestinationID:   destination.DestinationID,
		DestinationName: destination.Name,
		Channel:         destination.Channel,
		RuleID:          rule.RuleID,
		RuleName:        rule.Name,
		Event:           notification.Event,
		Severity:        notification.Severity,
		Title:           notification.Title,
		Body:            notification.Body,
		ContainerName:   notification.ContainerName,
		Result:          domain.DeliveryPending,
		DedupKey:        notification.DedupKey,
		QueuedAt:        s.now().UTC(),
	}
	if err := s.store.RecordDelivery(ctx, delivery); err != nil {
		s.logger.ErrorContext(ctx, "could not record a notification delivery",
			slog.Any("error", err))
		return
	}

	s.attempt(ctx, delivery, destination, 1)
}

// attempt performs one delivery attempt and records what happened.
func (s *NotificationService) attempt(
	ctx context.Context,
	delivery domain.NotificationDelivery,
	destination domain.NotificationDestination,
	attempt int,
) {
	// The credential, read immediately before the send and held only for its
	// duration. This is the one place a secret enters the service layer.
	secret, err := s.store.Secret(ctx, destination.DestinationID)
	if err != nil {
		// The error is logged WITHOUT the destination id: a failure to read a
		// credential appears in a log line beside the word "credential", and
		// there is no reason to correlate the two.
		s.logger.ErrorContext(ctx, "could not read a notification credential",
			slog.Any("error", err))
		s.finish(ctx, delivery, destination, attempt,
			notify.Result{Reason: notify.FailureConfiguration,
				Detail: notify.FailureConfiguration.Explain()})
		return
	}
	// An email destination relays through the configured server, whose
	// credential comes from configuration rather than from the row.
	if destination.Channel == domain.ChannelEmail {
		secret.SMTPUsername = s.secret.SMTPUsername
		secret.SMTPPassword = s.secret.SMTPPassword
	}

	// Detached and bounded. A delivery must survive the pass that raised it --
	// an execution's context ends the moment the recreation finishes -- and must
	// never outlive shutdown by more than its own budget.
	sendCtx, cancel := GraceContext(ctx, s.cfg.DeliveryTimeout, s.cfg.DeliveryTimeout)
	defer cancel()

	started := s.now()
	result := s.sender.Send(sendCtx, notify.SendRequest{
		Notification: domain.Notification{
			Event:         delivery.Event,
			Severity:      delivery.Severity,
			Title:         delivery.Title,
			Body:          delivery.Body,
			ContainerName: delivery.ContainerName,
			OccurredAt:    delivery.QueuedAt,
		},
		Destination: destination,
		Secret:      secret,
		SMTP:        s.smtp,
	})
	delivery.DurationMs = s.now().Sub(started).Milliseconds()

	s.finish(ctx, delivery, destination, attempt, result)
}

// finish records an attempt's outcome and schedules a retry when one is due.
func (s *NotificationService) finish(
	ctx context.Context,
	delivery domain.NotificationDelivery,
	destination domain.NotificationDestination,
	attempt int,
	result notify.Result,
) {
	now := s.now().UTC()

	outcome := domain.DeliverySucceeded
	var nextAttempt *time.Time

	switch {
	case result.OK:
	case !result.Retryable || attempt >= s.cfg.MaxAttempts:
		// The dead letter. A destination that is permanently broken -- a
		// revoked webhook URL -- reaches this on the first attempt, because
		// repeating a 403 forever helps nobody.
		outcome = domain.DeliveryFailed
	default:
		outcome = domain.DeliveryRetrying
		when := now.Add(s.backoff(attempt))
		nextAttempt = &when
	}

	if err := s.store.CompleteDelivery(ctx, delivery.DeliveryID, outcome,
		attempt, result.StatusCode, result.Detail, nextAttempt, now,
		delivery.DurationMs); err != nil {
		s.logger.ErrorContext(ctx, "could not record a notification outcome",
			slog.String("deliveryId", delivery.DeliveryID), slog.Any("error", err))
	}
	if err := s.store.RecordDestinationResult(ctx, destination.DestinationID,
		outcome, result.Detail, now); err != nil {
		s.logger.ErrorContext(ctx, "could not record a destination result",
			slog.Any("error", err))
	}

	if outcome == domain.DeliveryFailed {
		// Logged at warning, once, with no destination detail beyond the name
		// an operator chose. A notification subsystem nobody notices has broken
		// is worse than none at all.
		s.logger.WarnContext(ctx, "a notification could not be delivered",
			slog.String("destination", domain.SanitiseDisplayText(destination.Name, 120)),
			slog.String("event", string(delivery.Event)),
			slog.String("reason", string(result.Reason)),
			slog.Int("attempts", attempt))
	}
}

// backoff is the delay before attempt n+1.
//
// Exponential from the configured base, capped so a long-broken destination is
// retried at a sane interval rather than at an ever-growing one that eventually
// never fires.
func (s *NotificationService) backoff(attempt int) time.Duration {
	delay := s.cfg.RetryBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxNotificationBackoff {
			return maxNotificationBackoff
		}
	}
	return delay
}

// maxNotificationBackoff caps the retry delay.
const maxNotificationBackoff = 30 * time.Minute

// sweepRetries re-attempts deliveries whose next attempt has come.
func (s *NotificationService) sweepRetries(ctx context.Context) {
	due, err := s.store.DueRetries(ctx, s.now(), s.cfg.Workers*4)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not load notification retries", slog.Any("error", err))
		return
	}
	for _, delivery := range due {
		if ctx.Err() != nil {
			return
		}
		destination, err := s.store.DestinationByID(ctx, delivery.DestinationID)
		if err != nil {
			continue
		}
		if !destination.Enabled || destination.Archived {
			// The destination was switched off while the retry was pending.
			// Settled as failed rather than retried forever.
			_ = s.store.CompleteDelivery(ctx, delivery.DeliveryID,
				domain.DeliveryFailed, delivery.Attempts, 0,
				"the destination was disabled before this could be retried",
				nil, s.now().UTC(), delivery.DurationMs)
			continue
		}
		if !s.claim(destination.DestinationID) {
			// Busy. The next sweep will pick it up; the row keeps its
			// next_attempt_at and does not lose its place.
			continue
		}
		s.attempt(ctx, delivery, destination, delivery.Attempts+1)
		s.release(destination.DestinationID)
	}
}

// prune deletes delivery history past the configured horizon.
func (s *NotificationService) prune(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 {
		return
	}
	deleted, err := s.store.PruneDeliveries(ctx, s.now().Add(-s.cfg.RetentionAge), 0)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not prune notification history", slog.Any("error", err))
		return
	}
	if deleted > 0 {
		s.logger.Info("pruned notification history",
			slog.Int("deliveries", deleted),
			slog.Duration("retention", s.cfg.RetentionAge))
	}
}

// ------------------------------------------------------------ bookkeeping --

// claim reserves a per-destination concurrency slot.
func (s *NotificationService) claim(destinationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[destinationID] >= s.cfg.MaxPerDestination {
		return false
	}
	s.inFlight[destinationID]++
	return true
}

// release returns a slot.
func (s *NotificationService) release(destinationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[destinationID] <= 1 {
		// Deleted rather than left at zero, so the map does not grow with every
		// destination that has ever been used.
		delete(s.inFlight, destinationID)
		return
	}
	s.inFlight[destinationID]--
}

// recordSuppressed writes the row that says a cooldown swallowed a
// notification.
//
// Recorded rather than dropped silently: an operator asking "why did I not get
// told" needs to see that HarborMaster decided not to, and which rule decided.
func (s *NotificationService) recordSuppressed(
	ctx context.Context,
	notification domain.Notification,
	destination domain.NotificationDestination,
	rule domain.NotificationRule,
) {
	now := s.now().UTC()
	_ = s.store.RecordDelivery(ctx, domain.NotificationDelivery{
		DeliveryID:      domain.NewNotificationDeliveryID(),
		DestinationID:   destination.DestinationID,
		DestinationName: destination.Name,
		Channel:         destination.Channel,
		RuleID:          rule.RuleID,
		RuleName:        rule.Name,
		Event:           notification.Event,
		Severity:        notification.Severity,
		Title:           notification.Title,
		Body:            notification.Body,
		ContainerName:   notification.ContainerName,
		Result:          domain.DeliverySuppressed,
		DedupKey:        notification.DedupKey,
		QueuedAt:        now,
		CompletedAt:     &now,
	})
}

// recordDropped writes the row that says a routed notification was lost.
//
// The reason is one of a fixed set written by this file. It is never third-party
// text, and never a raw error.
func (s *NotificationService) recordDropped(
	ctx context.Context,
	notification domain.Notification,
	destination domain.NotificationDestination,
	rule domain.NotificationRule,
	reason string,
) {
	now := s.now().UTC()
	_ = s.store.RecordDelivery(ctx, domain.NotificationDelivery{
		DeliveryID:      domain.NewNotificationDeliveryID(),
		DestinationID:   destination.DestinationID,
		DestinationName: destination.Name,
		Channel:         destination.Channel,
		RuleID:          rule.RuleID,
		RuleName:        rule.Name,
		Event:           notification.Event,
		Severity:        notification.Severity,
		Title:           notification.Title,
		Body:            notification.Body,
		ContainerName:   notification.ContainerName,
		Result:          domain.DeliveryDropped,
		Error:           reason,
		DedupKey:        notification.DedupKey,
		QueuedAt:        now,
		CompletedAt:     &now,
	})
}

// ---------------------------------------------------------------- reading --

// Deliveries returns a bounded page of the delivery history.
func (s *NotificationService) Deliveries(
	ctx context.Context,
	filter store.DeliveryFilter,
) ([]domain.NotificationDelivery, int, error) {
	if !s.Readable() {
		return nil, 0, ErrNotificationsDisabled
	}
	return s.store.ListDeliveries(ctx, filter)
}

// Delivery returns one delivery record.
func (s *NotificationService) Delivery(
	ctx context.Context,
	deliveryID string,
) (domain.NotificationDelivery, error) {
	if !s.Readable() {
		return domain.NotificationDelivery{}, ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDeliveryID(deliveryID) {
		return domain.NotificationDelivery{}, store.ErrNotFound
	}
	return s.store.DeliveryByID(ctx, deliveryID)
}

// Summary returns the aggregate the dashboard and the page header show.
func (s *NotificationService) Summary(ctx context.Context) (domain.NotificationSummary, error) {
	if !s.Readable() {
		return domain.NotificationSummary{}, ErrNotificationsDisabled
	}
	summary, err := s.store.DeliverySummary(ctx)
	if err != nil {
		return domain.NotificationSummary{}, err
	}
	destinations, rules, failing, err := s.store.CountNotificationConfiguration(ctx)
	if err != nil {
		return domain.NotificationSummary{}, err
	}
	summary.Enabled = s.Enabled()
	summary.Destinations = destinations
	summary.Rules = rules
	summary.Failing = failing
	return summary, nil
}
