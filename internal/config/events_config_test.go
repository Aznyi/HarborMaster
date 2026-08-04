package config

import (
	"strings"
	"testing"
	"time"
)

// eventEnv builds a lookup function over a fixed map.
func eventEnv(values map[string]string) lookupFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestEventDefaults(t *testing.T) {
	cfg, err := load(eventEnv(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Events.Enabled {
		t.Error("the event engine must be on by default")
	}
	if cfg.Events.ReconnectInitial != DefaultEventReconnectInitial {
		t.Errorf("reconnectInitial = %s", cfg.Events.ReconnectInitial)
	}
	if cfg.Events.ReconnectMax != DefaultEventReconnectMax {
		t.Errorf("reconnectMax = %s", cfg.Events.ReconnectMax)
	}
	if cfg.Events.BufferSize != DefaultEventBufferSize {
		t.Errorf("bufferSize = %d", cfg.Events.BufferSize)
	}
	if cfg.Events.ReconcileInterval != DefaultEventReconcileInterval {
		t.Errorf("reconcileInterval = %s", cfg.Events.ReconcileInterval)
	}
	if cfg.Events.StreamSubscribers != DefaultEventStreamSubscribers {
		t.Errorf("streamSubscribers = %d", cfg.Events.StreamSubscribers)
	}
}

func TestEventSettingsAreReadFromTheEnvironment(t *testing.T) {
	cfg, err := load(eventEnv(map[string]string{
		"HARBORMASTER_EVENTS_ENABLED":                 "false",
		"HARBORMASTER_EVENTS_RECONNECT_INITIAL_DELAY": "2s",
		"HARBORMASTER_EVENTS_RECONNECT_MAX_DELAY":     "90s",
		"HARBORMASTER_EVENTS_RECONNECT_MULTIPLIER":    "1.5",
		"HARBORMASTER_EVENTS_BUFFER_SIZE":             "256",
		"HARBORMASTER_EVENTS_BATCH_SIZE":              "32",
		"HARBORMASTER_EVENTS_BATCH_FLUSH_INTERVAL":    "250ms",
		"HARBORMASTER_EVENTS_DEDUP_WINDOW":            "30s",
		"HARBORMASTER_EVENTS_REFRESH_DEBOUNCE":        "1s",
		"HARBORMASTER_EVENTS_RECONCILE_INTERVAL":      "5m",
		"HARBORMASTER_EVENTS_RETENTION_AGE":           "48h",
		"HARBORMASTER_EVENTS_RETENTION_COUNT":         "1000",
		"HARBORMASTER_EVENTS_PRUNE_INTERVAL":          "10m",
		"HARBORMASTER_EVENTS_STREAM_MAX_SUBSCRIBERS":  "4",
		"HARBORMASTER_EVENTS_STREAM_BUFFER_SIZE":      "64",
		"HARBORMASTER_EVENTS_STREAM_REPLAY_LIMIT":     "100",
		"HARBORMASTER_EVENTS_STREAM_HEARTBEAT":        "30s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	events := cfg.Events
	if events.Enabled {
		t.Error("EVENTS_ENABLED=false must switch the engine off")
	}
	if events.ReconnectInitial != 2*time.Second || events.ReconnectMax != 90*time.Second {
		t.Errorf("reconnect delays = %s/%s", events.ReconnectInitial, events.ReconnectMax)
	}
	if events.ReconnectFactor != 1.5 {
		t.Errorf("reconnectFactor = %v", events.ReconnectFactor)
	}
	if events.BufferSize != 256 || events.BatchSize != 32 {
		t.Errorf("buffer/batch = %d/%d", events.BufferSize, events.BatchSize)
	}
	if events.RetentionAge != 48*time.Hour || events.RetentionCount != 1000 {
		t.Errorf("retention = %s/%d", events.RetentionAge, events.RetentionCount)
	}
	if events.StreamSubscribers != 4 || events.StreamReplay != 100 {
		t.Errorf("stream limits = %d/%d", events.StreamSubscribers, events.StreamReplay)
	}
}

// Settings are validated even when the engine is off: an error that only
// surfaces the day someone enables it is a worse failure than a startup one.
func TestEventValidationRunsEvenWhenDisabled(t *testing.T) {
	_, err := load(eventEnv(map[string]string{
		"HARBORMASTER_EVENTS_ENABLED":     "false",
		"HARBORMASTER_EVENTS_BUFFER_SIZE": "0",
	}))
	if err == nil {
		t.Fatal("an invalid buffer size must be rejected even with the engine disabled")
	}
}

func TestEventValidationRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "reconnect delay below the minimum",
			env:  map[string]string{"HARBORMASTER_EVENTS_RECONNECT_INITIAL_DELAY": "1ms"},
			want: "EVENTS_RECONNECT_INITIAL_DELAY",
		},
		{
			name: "maximum smaller than the initial delay",
			env: map[string]string{
				"HARBORMASTER_EVENTS_RECONNECT_INITIAL_DELAY": "10s",
				"HARBORMASTER_EVENTS_RECONNECT_MAX_DELAY":     "1s",
			},
			want: "EVENTS_RECONNECT_MAX_DELAY",
		},
		{
			name: "multiplier below one would shrink the delay",
			env:  map[string]string{"HARBORMASTER_EVENTS_RECONNECT_MULTIPLIER": "0.5"},
			want: "EVENTS_RECONNECT_MULTIPLIER",
		},
		{
			name: "zero buffer",
			env:  map[string]string{"HARBORMASTER_EVENTS_BUFFER_SIZE": "0"},
			want: "EVENTS_BUFFER_SIZE",
		},
		{
			name: "batch larger than the queue",
			env: map[string]string{
				"HARBORMASTER_EVENTS_BUFFER_SIZE": "8",
				"HARBORMASTER_EVENTS_BATCH_SIZE":  "64",
			},
			want: "EVENTS_BATCH_SIZE",
		},
		{
			name: "reconcile interval too short",
			env:  map[string]string{"HARBORMASTER_EVENTS_RECONCILE_INTERVAL": "1s"},
			want: "EVENTS_RECONCILE_INTERVAL",
		},
		{
			name: "negative retention age",
			env:  map[string]string{"HARBORMASTER_EVENTS_RETENTION_AGE": "-1h"},
			want: "EVENTS_RETENTION_AGE",
		},
		{
			name: "negative retention count",
			env:  map[string]string{"HARBORMASTER_EVENTS_RETENTION_COUNT": "-5"},
			want: "EVENTS_RETENTION_COUNT",
		},
		{
			name: "subscriber limit above the ceiling",
			env:  map[string]string{"HARBORMASTER_EVENTS_STREAM_MAX_SUBSCRIBERS": "9999"},
			want: "EVENTS_STREAM_MAX_SUBSCRIBERS",
		},
		{
			name: "replay limit above the ceiling",
			env:  map[string]string{"HARBORMASTER_EVENTS_STREAM_REPLAY_LIMIT": "99999"},
			want: "EVENTS_STREAM_REPLAY_LIMIT",
		},
		{
			name: "non-numeric multiplier",
			env:  map[string]string{"HARBORMASTER_EVENTS_RECONNECT_MULTIPLIER": "fast"},
			want: "EVENTS_RECONNECT_MULTIPLIER",
		},
		{
			name: "non-duration dedup window",
			env:  map[string]string{"HARBORMASTER_EVENTS_DEDUP_WINDOW": "soon"},
			want: "EVENTS_DEDUP_WINDOW",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(eventEnv(tc.env))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %s", err, tc.want)
			}
			// The offending value must never be echoed back.
			for _, value := range tc.env {
				if value != "" && strings.Contains(err.Error(), value) && len(value) > 3 {
					t.Errorf("error echoed the supplied value %q: %q", value, err)
				}
			}
		})
	}
}

// Exactly one component owns the periodic full sweep. This is the decision
// point, so it is asserted directly rather than inferred from behaviour.
func TestReconcileIntervalOwnership(t *testing.T) {
	tests := []struct {
		name            string
		events          bool
		inventory       bool
		wantInterval    time.Duration
		wantEventEngine bool
	}{
		{
			name:   "engine on: it owns reconciliation",
			events: true, inventory: true,
			wantInterval: DefaultEventReconcileInterval, wantEventEngine: true,
		},
		{
			name:   "engine off: phase 2 behaviour is unchanged",
			events: false, inventory: true,
			wantInterval: DefaultRefreshInterval, wantEventEngine: false,
		},
		{
			name:   "inventory off: nothing to reconcile",
			events: true, inventory: false,
			wantInterval: DefaultRefreshInterval, wantEventEngine: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if !tc.events {
				env["HARBORMASTER_EVENTS_ENABLED"] = "false"
			}
			if !tc.inventory {
				env["HARBORMASTER_INVENTORY_ENABLED"] = "false"
			}

			cfg, err := load(eventEnv(env))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			interval, ownedByEvents := cfg.ReconcileInterval()
			if interval != tc.wantInterval {
				t.Errorf("interval = %s, want %s", interval, tc.wantInterval)
			}
			if ownedByEvents != tc.wantEventEngine {
				t.Errorf("ownedByEventEngine = %v, want %v", ownedByEvents, tc.wantEventEngine)
			}
		})
	}
}

// INVENTORY_REFRESH_INTERVAL is retained, not removed: an existing
// configuration must keep working exactly as it did.
func TestExistingInventoryIntervalStillGovernsWithoutTheEngine(t *testing.T) {
	cfg, err := load(eventEnv(map[string]string{
		"HARBORMASTER_EVENTS_ENABLED":             "false",
		"HARBORMASTER_INVENTORY_REFRESH_INTERVAL": "30s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	interval, ownedByEvents := cfg.ReconcileInterval()
	if interval != 30*time.Second || ownedByEvents {
		t.Errorf("interval = %s (events own it: %v), want 30s owned by the inventory",
			interval, ownedByEvents)
	}
}

// Config is a plausible carrier for secrets, so its rendering must stay
// value-free even as new sections are added.
func TestStringMentionsEventsWithoutValues(t *testing.T) {
	cfg, err := load(eventEnv(map[string]string{
		"HARBORMASTER_EVENTS_RECONCILE_INTERVAL": "7m",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rendered := cfg.String()
	if !strings.Contains(rendered, "events") {
		t.Errorf("String() = %q, want it to mention the events section", rendered)
	}
	if strings.Contains(rendered, "7m") {
		t.Errorf("String() leaked a configured value: %q", rendered)
	}
}
