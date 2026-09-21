package domain_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// How long a replacement is given to become healthy.
//
// Docker's own rule: failures during start_period do not count; after it,
// `retries` consecutive failures, each taking up to `timeout` and spaced
// `interval` apart, make the container unhealthy. So the daemon can take
//
//	start_period + retries * (interval + timeout)
//
// to reach a verdict, and a container HarborMaster gave less than that could be
// rolled back while Docker still calls it starting. The deadline is the larger
// of that and HarborMaster's configured startup timeout, capped so a malformed
// healthcheck cannot hold an update transaction open indefinitely.

func TestTheHealthDeadlineFollowsTheHealthcheck(t *testing.T) {
	const configured = 5 * time.Minute
	const cap = 30 * time.Minute

	cases := []struct {
		name  string
		check *domain.HealthCheck
		want  time.Duration
	}{
		{"no healthcheck", nil, configured},
		{"disabled healthcheck", &domain.HealthCheck{Disabled: true, Test: []string{"NONE"}}, configured},
		{"short healthcheck", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, IntervalMS: 1000, TimeoutMS: 1000, Retries: 2,
		}, configured},
		{"long start period", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, StartPeriodMS: int64((10 * time.Minute) / time.Millisecond),
			IntervalMS: 10000, TimeoutMS: 5000, Retries: 3,
		}, 10*time.Minute + 3*(10*time.Second+5*time.Second)},
		{"very long interval", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, IntervalMS: int64((4 * time.Minute) / time.Millisecond),
			TimeoutMS: 30000, Retries: 3,
		}, 3 * (4*time.Minute + 30*time.Second)},
		// Zero values are Docker's defaults: interval 30s, timeout 30s,
		// retries 3, start period 0 -- three minutes, under the configured
		// five.
		{"docker defaults", &domain.HealthCheck{Test: []string{"CMD", "true"}}, configured},
		{"configured timeout is longer", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, IntervalMS: 5000, TimeoutMS: 5000, Retries: 3,
		}, configured},
		{"healthcheck is longer than the default", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, StartPeriodMS: int64((6 * time.Minute) / time.Millisecond),
			IntervalMS: 30000, TimeoutMS: 30000, Retries: 3,
		}, 6*time.Minute + 3*time.Minute},
		{"capped", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, StartPeriodMS: int64((2 * time.Hour) / time.Millisecond),
			IntervalMS: 30000, TimeoutMS: 30000, Retries: 3,
		}, cap},
		{"absurd values are capped", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, IntervalMS: 1 << 62, TimeoutMS: 1 << 62, Retries: 1 << 30,
		}, cap},
		{"negative values fall back to the defaults", &domain.HealthCheck{
			Test: []string{"CMD", "true"}, IntervalMS: -1, TimeoutMS: -1, Retries: -1, StartPeriodMS: -1,
		}, configured},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := domain.HealthDeadline(testCase.check, configured, cap)
			if got != testCase.want {
				t.Errorf("HealthDeadline = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestTheHealthDeadlineIsNeverBelowTheConfiguredTimeoutOrAboveTheCap pins the
// two bounds independently of any healthcheck shape.
func TestTheHealthDeadlineIsNeverBelowTheConfiguredTimeoutOrAboveTheCap(t *testing.T) {
	check := &domain.HealthCheck{Test: []string{"CMD", "true"}, StartPeriodMS: 1, IntervalMS: 1, TimeoutMS: 1, Retries: 1}
	if got := domain.HealthDeadline(check, time.Minute, time.Hour); got != time.Minute {
		t.Errorf("a tiny healthcheck lowered the deadline to %v", got)
	}
	// A cap below the configured timeout is a configuration error; the
	// configured timeout still wins, so an operator's setting is honoured.
	if got := domain.HealthDeadline(check, 10*time.Minute, time.Minute); got != 10*time.Minute {
		t.Errorf("a cap below the configured timeout produced %v", got)
	}
}
