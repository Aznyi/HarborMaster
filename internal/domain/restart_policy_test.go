package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Restart policies, as HarborMaster reasons about them when it parks a
// container.
//
// # The rule, and why it is uniform
//
// Docker's restart manager restarts a stopped container after a daemon restart
// under `always` unconditionally, and under `unless-stopped` unless the
// container was stopped by an explicit stop -- a fact the daemon keeps to
// itself and never reports through inspect. `on-failure` is not restored on a
// daemon restart at all, but a container in restart backoff at the moment it
// is quarantined is still governed by it. HarborMaster cannot observe which of
// those states a parked container is in, so it does not try: every policy that
// can restart a container without a person asking is suspended, and only the
// policy that never restarts anything is left alone.

func TestEveryPolicyThatCanRestartUnattendedIsSuspended(t *testing.T) {
	cases := []struct {
		policy domain.RestartPolicy
		want   bool
	}{
		{domain.RestartPolicy{}, false},
		{domain.RestartPolicy{Name: "no"}, false},
		{domain.RestartPolicy{Name: "always"}, true},
		{domain.RestartPolicy{Name: "unless-stopped"}, true},
		{domain.RestartPolicy{Name: "on-failure"}, true},
		{domain.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}, true},
	}
	for _, testCase := range cases {
		if got := testCase.policy.RestartsUnattended(); got != testCase.want {
			t.Errorf("%+v RestartsUnattended() = %v, want %v", testCase.policy, got, testCase.want)
		}
	}
}

func TestARestartPolicySurvivesARoundTripThroughItsRecordedForm(t *testing.T) {
	cases := []domain.RestartPolicy{
		{Name: "no"},
		{Name: "always"},
		{Name: "unless-stopped"},
		{Name: "on-failure"},
		{Name: "on-failure", MaximumRetryCount: 5},
	}
	for _, policy := range cases {
		encoded := policy.Encode()
		decoded, ok := domain.ParseRestartPolicy(encoded)
		if !ok {
			t.Errorf("%q did not parse back", encoded)
			continue
		}
		if decoded != policy {
			t.Errorf("%+v encoded as %q and decoded as %+v", policy, encoded, decoded)
		}
	}

	// The daemon's "unset" is HarborMaster's "no".
	if got := (domain.RestartPolicy{}).Encode(); got != "no" {
		t.Errorf("an unset policy encodes as %q, want no", got)
	}
	// A value that could only have come from somewhere other than this file is
	// refused rather than handed to the daemon.
	for _, bad := range []string{"", "sometimes", "always:3", "on-failure:x", "on-failure:-1"} {
		if _, ok := domain.ParseRestartPolicy(bad); ok {
			t.Errorf("%q parsed as a restart policy", bad)
		}
	}
}
