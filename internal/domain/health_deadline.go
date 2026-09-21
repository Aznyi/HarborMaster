package domain

import "time"

// How long a replacement is given to prove itself healthy.
//
// # Docker's rule
//
// A healthcheck runs every `interval`; each run may take up to `timeout`.
// Failures during `start_period` do not count. After it, `retries`
// consecutive failures mark the container unhealthy; a single success marks it
// healthy. So the LONGEST the daemon can legitimately take before it reaches
// a verdict is
//
//	start_period + retries × (interval + timeout)
//
// and a replacement given less than that can be quarantined while Docker still
// calls it "starting" -- a container behaving exactly as configured, rolled
// back for being slow to say so.
//
// # HarborMaster's rule
//
// The deadline is the larger of the configured startup timeout and the
// healthcheck's own budget, and never more than the configured cap. The
// operator's timeout is a floor, so a short healthcheck never shortens the
// wait; the cap is a ceiling, so a malformed or hostile healthcheck -- a
// start period of a year, an interval that overflows -- cannot hold an update
// transaction open. Every wait in HarborMaster is bounded, and this one is
// bounded twice.

// Docker's defaults for a healthcheck field left at zero.
const (
	DefaultHealthcheckInterval = 30 * time.Second
	DefaultHealthcheckTimeout  = 30 * time.Second
	DefaultHealthcheckRetries  = 3
)

// HealthDeadline returns how long to wait for a health verdict.
//
// check may be nil or disabled, in which case there is no healthcheck budget
// and the configured timeout stands. A cap below the configured timeout is
// treated as equal to it: an operator's explicit timeout is always honoured.
func HealthDeadline(check *HealthCheck, configured, cap time.Duration) time.Duration {
	if cap < configured {
		cap = configured
	}
	if check == nil || check.Disabled || len(check.Test) == 0 {
		return configured
	}
	if len(check.Test) == 1 && check.Test[0] == "NONE" {
		return configured
	}

	budget := healthcheckBudget(check)
	if budget < configured {
		return configured
	}
	if budget > cap {
		return cap
	}
	return budget
}

// healthcheckBudget is the daemon's worst-case time to a verdict, with zero
// or negative fields taking Docker's defaults and arithmetic that cannot
// overflow into a small number.
func healthcheckBudget(check *HealthCheck) time.Duration {
	interval := millisOrDefault(check.IntervalMS, DefaultHealthcheckInterval)
	timeout := millisOrDefault(check.TimeoutMS, DefaultHealthcheckTimeout)
	start := millisOrDefault(check.StartPeriodMS, 0)
	retries := check.Retries
	if retries <= 0 {
		retries = DefaultHealthcheckRetries
	}

	// Saturating arithmetic. A value large enough to overflow a duration is
	// a value the cap will clamp anyway; it must not wrap into a small one.
	perAttempt := saturatingAdd(interval, timeout)
	attempts := saturatingMul(perAttempt, retries)
	return saturatingAdd(start, attempts)
}

func millisOrDefault(millis int64, fallback time.Duration) time.Duration {
	if millis <= 0 {
		return fallback
	}
	const maxMillis = int64(1<<63-1) / int64(time.Millisecond)
	if millis > maxMillis {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(millis) * time.Millisecond
}

func saturatingAdd(a, b time.Duration) time.Duration {
	const max = time.Duration(1<<63 - 1)
	if a > max-b {
		return max
	}
	return a + b
}

func saturatingMul(a time.Duration, n int) time.Duration {
	const max = time.Duration(1<<63 - 1)
	if n <= 0 {
		return 0
	}
	if a > max/time.Duration(n) {
		return max
	}
	return a * time.Duration(n)
}
