package service

import (
	"context"
	"sync"
	"time"
)

// Shutdown discipline for detached background work.
//
// Two requirements pull in opposite directions, and both are real:
//
//   - A sweep that is mid-transaction must not be cancelled by a signal. A
//     refresh cancelled between two writes leaves the inventory describing a
//     moment that never existed, and the whole point of committing a refresh
//     in one transaction is to make that impossible.
//   - Nothing may outlive the shutdown grace period. The orchestrator sends
//     SIGTERM, waits, and then SIGKILLs. Work that ignores cancellation does
//     not get "as long as it needs"; it gets killed, at an arbitrary point,
//     which is the outcome the first requirement was trying to avoid.
//
// Detaching from cancellation satisfies the first and violates the second. A
// refresh detached with context.WithoutCancel and a fifteen-minute bound will,
// on a large host, hold the process open for fifteen minutes after SIGTERM --
// or, more realistically, until the runtime kills it.
//
// GraceContext satisfies both: the work survives the signal for a BOUNDED
// grace period, long enough to commit what is in flight, and is cancelled
// after it. A commit that cannot finish inside the grace period is rolled back
// by SQLite, which is exactly what would have happened anyway, only now it
// happens predictably instead of at whatever instant SIGKILL arrives.

// defaultDetachedMax bounds detached work when the caller names no maximum.
const defaultDetachedMax = 15 * time.Minute

// DefaultShutdownGrace is how long detached work may run past cancellation
// when nothing more specific is configured.
//
// It matches the server's default shutdown timeout deliberately: the HTTP
// drain and the background drain share one budget, because they share one
// orchestrator-imposed deadline.
const DefaultShutdownGrace = 15 * time.Second

// GraceContext returns a context that survives parent's cancellation for at
// most grace, and is bounded by max regardless.
//
// It is the tool for work that must not be interrupted mid-transaction but
// must not outlive shutdown either. The returned CancelFunc must be called; it
// releases the watchdog goroutine as well as the context.
//
// grace <= 0 means no grace at all, in which case the returned context is an
// ordinary child of parent bounded by max, and no goroutine is started.
func GraceContext(parent context.Context, grace, max time.Duration) (context.Context, context.CancelFunc) {
	if max <= 0 {
		max = defaultDetachedMax
	}
	if grace <= 0 {
		return context.WithTimeout(parent, max)
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), max)

	stop := make(chan struct{})
	var once sync.Once
	release := func() {
		// Once, because a CancelFunc may be called any number of times and
		// closing a closed channel panics.
		once.Do(func() { close(stop) })
		cancel()
	}

	go func() {
		// The watchdog cannot outlive the context it guards: ctx always ends,
		// at the latest when max elapses.
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-parent.Done():
		}

		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case <-timer.C:
			cancel()
		case <-stop:
		case <-ctx.Done():
		}
	}()

	return ctx, release
}

// WaitGroupTimeout waits for wg, reporting false if timeout elapsed first.
//
// A bounded wait, because sync.WaitGroup offers none and an unbounded one at
// shutdown means a single wedged goroutine can hold the whole process open. A
// caller that gets false has learned something worth logging: it should say
// what it is abandoning rather than wait silently.
//
// The internal waiter goroutine outlives a timed-out call and ends when the
// group finally drains. That is deliberate: the alternative is to abandon the
// group's own accounting, and the process is on its way out regardless.
func WaitGroupTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
