package docker

import (
	"context"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// FakeAcquirer is an in-memory ImageAcquirer for tests.
//
// Deliberately SEPARATE from Fake rather than a few more fields on it. Fake is
// handed to every service that observes Docker, and if it also satisfied
// ImageAcquirer then every one of those tests would be exercising a double that
// can mutate. Keeping them apart means a test has the pull capability only when
// it asked for it -- the same property the production wiring has.
//
// It models the cases that matter and are hard to produce against a real
// daemon: a pull that succeeds but delivers the wrong content, one that hangs
// until cancelled, one that fails transiently, and one that emits hostile
// progress text.
type FakeAcquirer struct {
	mu sync.Mutex

	// Err fails the pull outright.
	Err error
	// ErrAfter, when positive, fails the pull after that many progress
	// messages -- a transfer that dies mid-flight rather than at the request.
	ErrAfter int

	// Progress is emitted in order, subject to ErrAfter.
	Progress []PullProgress
	// Delay is slept between progress messages, so a test can cancel a pull
	// that is genuinely in flight.
	Delay time.Duration

	// Result is returned on success.
	Result PullResult

	// Targets records every pull that was requested, which is what proves a
	// refused acquisition never reached the daemon.
	Targets []PullTarget
	// Calls counts pull attempts.
	Calls int

	// Started is closed on the first pull, so a test can wait for one to be in
	// flight without sleeping.
	Started chan struct{}
}

// NewFakeAcquirer builds a fake that succeeds immediately.
func NewFakeAcquirer() *FakeAcquirer {
	return &FakeAcquirer{Started: make(chan struct{}, 8)}
}

// PullByDigest records the request and replays the configured behaviour.
//
// It performs the SAME target validation the real client does, so a test cannot
// accidentally prove that an illegal target is acceptable.
func (f *FakeAcquirer) PullByDigest(
	ctx context.Context,
	target PullTarget,
	progress func(PullProgress),
) (PullResult, error) {
	if err := target.Validate(); err != nil {
		return PullResult{}, err
	}

	f.mu.Lock()
	f.Calls++
	f.Targets = append(f.Targets, target)
	err, errAfter, delay := f.Err, f.ErrAfter, f.Delay
	messages := append([]PullProgress(nil), f.Progress...)
	result := f.Result
	started := f.Started
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	if err != nil && errAfter == 0 {
		return PullResult{}, err
	}

	for index, message := range messages {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return PullResult{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		if ctx.Err() != nil {
			return PullResult{}, ctx.Err()
		}
		if progress != nil {
			progress(message)
		}
		if errAfter > 0 && index+1 >= errAfter {
			if err == nil {
				err = ErrPullFailed
			}
			return PullResult{}, err
		}
	}

	if ctx.Err() != nil {
		return PullResult{}, ctx.Err()
	}
	if result.Reference == "" {
		result.Reference = target.Reference()
	}
	return result, nil
}

// CallCount reports how many pulls were attempted.
func (f *FakeAcquirer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}

// LastTarget returns the most recent pull target.
func (f *FakeAcquirer) LastTarget() (PullTarget, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Targets) == 0 {
		return PullTarget{}, false
	}
	return f.Targets[len(f.Targets)-1], true
}

// SetErr configures the failure returned by subsequent pulls.
func (f *FakeAcquirer) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// Compile-time check that the fake really is the mutation capability.
var _ ImageAcquirer = (*FakeAcquirer)(nil)

// ProgressFor is a convenience for building a plausible progress sequence.
func ProgressFor(status string, layers int) []PullProgress {
	out := make([]PullProgress, 0, layers)
	for index := 0; index < layers; index++ {
		out = append(out, PullProgress{
			Status:  domain.SanitiseDisplayText(status, maxProgressStatusBytes),
			Current: int64(index+1) * 1024,
			Total:   int64(layers) * 1024,
			Layers:  index + 1,
		})
	}
	return out
}
