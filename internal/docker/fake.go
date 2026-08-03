package docker

import "context"

// Fake is an in-memory Pinger for tests. It lives in the production package so
// that both the service and API test suites can share one double.
type Fake struct {
	// Info is returned when Err is nil.
	Info Info
	// Err, when set, is returned from Ping.
	Err error
	// Calls counts Ping invocations.
	Calls int
}

// Ping implements Pinger.
func (f *Fake) Ping(_ context.Context) (Info, error) {
	f.Calls++
	if f.Err != nil {
		return Info{}, f.Err
	}
	return f.Info, nil
}

var _ Pinger = (*Fake)(nil)
