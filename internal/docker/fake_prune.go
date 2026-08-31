package docker

import (
	"context"
	"sync"
)

// FakeImagePruner is an in-memory ImagePruner for tests.
//
// Deliberately SEPARATE from Fake, FakeAcquirer and FakeMutator, for the reason
// those are separate from each other: a test holds the capability only when it
// asked for one. A service handed only a Fake cannot remove an image in a test
// any more than it can in production.
//
// It models the three answers the daemon actually gives -- removed, already
// gone, still in use -- because the difference between them is the whole of
// what a cleanup pass has to handle correctly.
type FakeImagePruner struct {
	mu sync.Mutex

	// Present is the modelled image store. An id absent from it is already
	// gone; an id mapped to true is in use and will be refused.
	Present map[string]bool
	// InUse marks images the daemon will refuse to remove.
	InUse map[string]bool
	// Err fails the NEXT call, for the genuine-failure path.
	Err error
	// Unavailable makes every call fail as though the daemon were down.
	Unavailable bool

	// Removed records every id actually removed, in order. The one thing most
	// tests assert on: cleanup safety is almost entirely a statement about
	// which images were removed and which were not.
	Removed []string
	// Requested records every id asked about, removed or not, so a test can
	// distinguish "never considered" from "considered and refused".
	Requested []string
}

// NewFakeImagePruner returns a pruner with an empty modelled store.
func NewFakeImagePruner() *FakeImagePruner {
	return &FakeImagePruner{
		Present: map[string]bool{},
		InUse:   map[string]bool{},
	}
}

// Add puts an image into the modelled store.
func (f *FakeImagePruner) Add(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.Present[id] = true
	}
}

// MarkInUse makes the daemon refuse to remove an image.
func (f *FakeImagePruner) MarkInUse(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.InUse[id] = true
	}
}

// RemoveImage models the daemon.
//
// Validates exactly as the real client does, so a test cannot pass an
// identifier production would refuse.
func (f *FakeImagePruner) RemoveImage(
	_ context.Context, request ImageRemoveRequest,
) (ImageRemoveOutcome, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.Requested = append(f.Requested, request.ImageID)

	if f.Unavailable {
		return "", ErrUnreachable
	}
	if f.Err != nil {
		err := f.Err
		f.Err = nil
		return "", err
	}
	if f.InUse[request.ImageID] {
		// The daemon's refusal. Never escalated: there is no force to try.
		return ImageStillInUse, nil
	}
	if !f.Present[request.ImageID] {
		return ImageAlreadyGone, nil
	}

	delete(f.Present, request.ImageID)
	f.Removed = append(f.Removed, request.ImageID)
	return ImageRemoved, nil
}

// RemovedIDs returns the images removed so far.
func (f *FakeImagePruner) RemovedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Removed...)
}

// RequestedIDs returns every image asked about.
func (f *FakeImagePruner) RequestedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Requested...)
}

// Holds reports whether the modelled store still has an image.
func (f *FakeImagePruner) Holds(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Present[id]
}

var _ ImagePruner = (*FakeImagePruner)(nil)
