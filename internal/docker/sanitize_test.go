package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// SanitizeError's timeout and cancellation branches were unreachable before the
// security audit: the Docker calls wrapped their cause with %v rather than %w,
// so errors.Is could never see past ErrUnreachable and every failure rendered
// as a flat "unreachable".
//
// The bug was invisible because the fallback was also the safe answer. These
// tests make the branches observable so they cannot quietly die again.
func TestSanitizeErrorDistinguishesFailureModes(t *testing.T) {
	// Exactly the shape the Docker calls produce.
	wrap := func(cause error) error {
		return fmt.Errorf("%w: %w", ErrUnreachable, cause)
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{
			"deadline exceeded",
			wrap(context.DeadlineExceeded),
			"docker engine did not respond in time",
		},
		{
			"cancelled",
			wrap(context.Canceled),
			"docker probe cancelled",
		},
		{
			"connection refused",
			wrap(errors.New("dial unix /var/run/docker.sock: connect: connection refused")),
			ErrUnreachable.Error(),
		},
		{
			"bare error",
			errors.New("something else"),
			ErrUnreachable.Error(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeError(tc.err); got != tc.want {
				t.Errorf("SanitizeError = %q, want %q", got, tc.want)
			}
		})
	}
}

// Wrapping the cause must not widen what a caller sees. The sanitised string is
// a fixed phrase whatever the cause said.
func TestSanitizeErrorNeverLeaksDaemonDetail(t *testing.T) {
	leaky := fmt.Errorf("%w: %w", ErrUnreachable,
		errors.New("dial unix /var/run/docker.sock: connect: permission denied (uid=65532)"))

	sanitised := SanitizeError(leaky)

	for _, secret := range []string{
		"/var/run/docker.sock", "docker.sock", "permission denied", "uid=65532", "dial unix",
	} {
		if strings.Contains(sanitised, secret) {
			t.Errorf("SanitizeError leaked %q: %s", secret, sanitised)
		}
	}

	// The detail is still available for the log, which is the point of keeping
	// it in the error at all.
	if !strings.Contains(leaky.Error(), "docker.sock") {
		t.Error("the wrapped error lost the detail the logs need")
	}
}

// The sentinel must remain matchable through the wrap, since callers branch on
// it to tell "Docker is down" from "the query failed".
func TestErrUnreachableRemainsMatchable(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrUnreachable, context.DeadlineExceeded)

	if !errors.Is(err, ErrUnreachable) {
		t.Error("ErrUnreachable is not matchable through the wrap")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the cause is not matchable through the wrap")
	}
}
