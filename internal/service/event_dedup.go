package service

import (
	"sync"
	"time"
)

// dedupWindow remembers recently seen event fingerprints.
//
// Docker can deliver the same event twice: a reconnect that resumes from a
// timestamp overlapping what was already read is the common case, and a
// duplicate delivery is permitted by the protocol besides. Re-processing one
// would mean a second refresh for a state that did not change.
//
// What this must NOT do is suppress a genuinely repeated action. Two `start`
// events for the same container at different instants are two real state
// transitions, and they carry different fingerprints (see
// docker.EventFingerprint), so they are never merged. Only a byte-identical
// identity inside the window is suppressed.
//
// Memory is bounded two ways: entries expire after the window, and the map is
// hard-capped. Without the cap, a burst faster than the sweep could grow the
// map without limit, which is exactly the failure mode an event engine on a
// busy host must not have.
type dedupWindow struct {
	mu sync.Mutex

	window  time.Duration
	maxSize int
	seen    map[string]time.Time
	// lastSweep throttles expiry so a burst does not walk the whole map per
	// event. Sweeping is O(n); doing it once per window rather than once per
	// event is what keeps admission O(1) amortised.
	lastSweep time.Time
}

// newDedupWindow builds a window. A non-positive duration yields a window that
// admits everything, which is a valid way to switch deduplication off.
func newDedupWindow(window time.Duration, maxSize int) *dedupWindow {
	if maxSize < 1 {
		maxSize = 1
	}
	return &dedupWindow{
		window:  window,
		maxSize: maxSize,
		seen:    make(map[string]time.Time),
	}
}

// admit reports whether an event should be processed, recording it when so.
//
// Returns false for a fingerprint already inside the window.
func (d *dedupWindow) admit(fingerprint string, now time.Time) bool {
	if d.window <= 0 || fingerprint == "" {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.sweepLocked(now)

	if seenAt, duplicate := d.seen[fingerprint]; duplicate {
		if now.Sub(seenAt) < d.window {
			return false
		}
	}

	// At capacity, evict before inserting. Eviction drops the oldest entries,
	// which are the ones least likely to be duplicated again.
	if len(d.seen) >= d.maxSize {
		d.evictOldestLocked(len(d.seen) - d.maxSize + 1)
	}

	d.seen[fingerprint] = now
	return true
}

// size reports how many fingerprints are held. Test-facing.
func (d *dedupWindow) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// sweepLocked drops expired entries, at most once per window.
func (d *dedupWindow) sweepLocked(now time.Time) {
	if !d.lastSweep.IsZero() && now.Sub(d.lastSweep) < d.window {
		return
	}
	d.lastSweep = now

	for fingerprint, seenAt := range d.seen {
		if now.Sub(seenAt) >= d.window {
			delete(d.seen, fingerprint)
		}
	}
}

// evictOldestLocked removes the n oldest entries.
//
// A linear scan per eviction rather than a heap: evictions only happen at the
// hard cap, which a correctly configured engine never reaches, and a heap would
// add complexity to the common path to speed up the pathological one.
func (d *dedupWindow) evictOldestLocked(n int) {
	for ; n > 0; n-- {
		var (
			oldestKey  string
			oldestTime time.Time
			found      bool
		)
		for fingerprint, seenAt := range d.seen {
			if !found || seenAt.Before(oldestTime) {
				oldestKey, oldestTime, found = fingerprint, seenAt, true
			}
		}
		if !found {
			return
		}
		delete(d.seen, oldestKey)
	}
}
