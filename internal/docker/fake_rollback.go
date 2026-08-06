package docker

import (
	"context"
)

// The rollback capability's test double.
//
// # Why it is the SAME modelled host as FakeMutator
//
// A rollback acts on containers a recreation created and parked. A test that
// exercises both against two separate models would be testing an arrangement
// that cannot occur: the recreation's replacement would not exist on the
// rollback's host.
//
// So FakeMutator implements ContainerRollbacker as well, over one map of
// containers and one call log. A test can run a recreation to a failure and
// then roll it back, and the assertions about which operations happened in what
// order -- which is what almost every safety property here reduces to -- are
// made against a single ordered record.
//
// # It validates exactly what the real client validates
//
// Each method calls the same Validate as the Client, so a request the adapter
// would refuse is refused here too. A test that got past the fake with an
// illegal request would be a test proving nothing.

// StopReplacement stops the modelled replacement.
func (f *FakeMutator) StopReplacement(ctx context.Context, request RollbackStopRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return f.StopContainer(ctx, StopRequest{
		ContainerID: request.ReplacementID,
		Timeout:     request.Timeout,
	})
}

// ParkReplacement renames the modelled replacement aside.
func (f *FakeMutator) ParkReplacement(ctx context.Context, request RollbackParkRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return f.RenameContainer(ctx, RenameRequest{
		ContainerID: request.ReplacementID,
		NewName:     request.ParkedName,
	})
}

// RestoreOriginalName renames the modelled original back.
func (f *FakeMutator) RestoreOriginalName(ctx context.Context, request RollbackRestoreRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return f.RenameContainer(ctx, RenameRequest{
		ContainerID: request.OriginalID,
		NewName:     request.Name,
	})
}

// StartOriginal starts the modelled original.
func (f *FakeMutator) StartOriginal(ctx context.Context, request RollbackStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return f.StartContainer(ctx, StartRequest{ContainerID: request.OriginalID})
}
