package config

import (
	"strings"
	"testing"
	"time"
)

// envMap builds a lookupFunc over a fixed map so tests never mutate the real
// process environment.
func envMap(kv map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

func TestLoadDefaultsBindToLoopback(t *testing.T) {
	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load with empty environment: %v", err)
	}

	if cfg.Server.Addr != DefaultHTTPAddr {
		t.Errorf("Addr = %q, want %q", cfg.Server.Addr, DefaultHTTPAddr)
	}
	if !cfg.IsLoopback() {
		t.Error("default listener must be loopback so a bare binary is not network exposed")
	}
	if cfg.Server.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("MaxRequestBytes = %d, want %d", cfg.Server.MaxRequestBytes, DefaultMaxRequestBytes)
	}
	if cfg.Log.Level != DefaultLogLevel || cfg.Log.Format != DefaultLogFormat {
		t.Errorf("log defaults = %q/%q, want %q/%q",
			cfg.Log.Level, cfg.Log.Format, DefaultLogLevel, DefaultLogFormat)
	}
}

func TestLoadEveryTimeoutHasAPositiveDefault(t *testing.T) {
	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	timeouts := map[string]time.Duration{
		"ReadHeaderTimeout": cfg.Server.ReadHeaderTimeout,
		"ReadTimeout":       cfg.Server.ReadTimeout,
		"WriteTimeout":      cfg.Server.WriteTimeout,
		"IdleTimeout":       cfg.Server.IdleTimeout,
		"ShutdownTimeout":   cfg.Server.ShutdownTimeout,
		"DockerTimeout":     cfg.Docker.Timeout,
	}
	for name, d := range timeouts {
		if d <= 0 {
			t.Errorf("%s = %v, want a positive default", name, d)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(envMap(map[string]string{
		"HARBORMASTER_HTTP_ADDR":         "0.0.0.0:9000",
		"HARBORMASTER_READ_TIMEOUT":      "42s",
		"HARBORMASTER_MAX_REQUEST_BYTES": "2048",
		"HARBORMASTER_DOCKER_HOST":       "unix:///tmp/docker.sock",
		"HARBORMASTER_DB_PATH":           "/var/lib/hm/hm.db",
		"HARBORMASTER_LOG_LEVEL":         "DEBUG",
		"HARBORMASTER_LOG_FORMAT":        "text",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Server.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 42*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.MaxRequestBytes != 2048 {
		t.Errorf("MaxRequestBytes = %d", cfg.Server.MaxRequestBytes)
	}
	if cfg.Store.Path != "/var/lib/hm/hm.db" {
		t.Errorf("DB path = %q", cfg.Store.Path)
	}
	// Case is normalised so operators can write DEBUG or debug.
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.IsLoopback() {
		t.Error("0.0.0.0 must not be reported as loopback")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := map[string]map[string]string{
		"bad address":  {"HARBORMASTER_HTTP_ADDR": "not-an-address"},
		"bad duration": {"HARBORMASTER_READ_TIMEOUT": "soon"},
		"bad integer":  {"HARBORMASTER_MAX_REQUEST_BYTES": "lots"},
		"zero body":    {"HARBORMASTER_MAX_REQUEST_BYTES": "0"},
		"bad level":    {"HARBORMASTER_LOG_LEVEL": "verbose"},
		"bad format":   {"HARBORMASTER_LOG_FORMAT": "xml"},
	}

	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := load(envMap(env)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// A malformed value must not be echoed back: config errors surface in logs and
// on stderr, and the variable could hold a credential later on.
func TestLoadErrorsDoNotLeakValues(t *testing.T) {
	const secret = "super-secret-value"

	_, err := load(envMap(map[string]string{"HARBORMASTER_READ_TIMEOUT": secret}))
	if err == nil {
		t.Fatal("expected an error for a malformed duration")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaked the environment value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "HARBORMASTER_READ_TIMEOUT") {
		t.Errorf("error should name the offending variable, got %q", err.Error())
	}
}

// String is the only rendering of Config, and it must stay value-free.
func TestStringRedactsEverything(t *testing.T) {
	cfg, err := load(envMap(map[string]string{
		"HARBORMASTER_DB_PATH":     "/secret/path/hm.db",
		"HARBORMASTER_DOCKER_HOST": "unix:///secret/docker.sock",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rendered := cfg.String()
	for _, leak := range []string{"/secret/path/hm.db", "unix:///secret/docker.sock", cfg.Server.Addr} {
		if strings.Contains(rendered, leak) {
			t.Errorf("Config.String() leaked %q: %s", leak, rendered)
		}
	}
}

func TestHealthcheckTimeoutDefaultsAndOverrides(t *testing.T) {
	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Healthcheck.Timeout != DefaultHealthcheckTimeout {
		t.Errorf("timeout = %v, want %v", cfg.Healthcheck.Timeout, DefaultHealthcheckTimeout)
	}

	overridden, err := load(envMap(map[string]string{"HARBORMASTER_HEALTHCHECK_TIMEOUT": "750ms"}))
	if err != nil {
		t.Fatalf("load with override: %v", err)
	}
	if overridden.Healthcheck.Timeout != 750*time.Millisecond {
		t.Errorf("timeout = %v, want 750ms", overridden.Healthcheck.Timeout)
	}

	if _, err := load(envMap(map[string]string{"HARBORMASTER_HEALTHCHECK_TIMEOUT": "0s"})); err == nil {
		t.Error("a zero healthcheck timeout must be rejected")
	}
}

// The container health check must target the port the server actually binds,
// so the URL is derived from the listen address rather than configured twice.
func TestHealthcheckURL(t *testing.T) {
	tests := map[string]string{
		// A wildcard bind is not dialable; it has to become loopback.
		"0.0.0.0:8080":   "http://127.0.0.1:8080/api/v1/health",
		"[::]:8080":      "http://127.0.0.1:8080/api/v1/health",
		"127.0.0.1:8080": "http://127.0.0.1:8080/api/v1/health",
		// A non-default port must be carried through.
		"0.0.0.0:9999": "http://127.0.0.1:9999/api/v1/health",
		// A specific interface is dialable as-is.
		"10.0.0.5:8080": "http://10.0.0.5:8080/api/v1/health",
		// IPv6 literals need brackets to be a valid URL host.
		"[::1]:8080": "http://[::1]:8080/api/v1/health",
	}

	for addr, want := range tests {
		cfg := Config{Server: Server{Addr: addr}}
		if got := cfg.HealthcheckURL(); got != want {
			t.Errorf("HealthcheckURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestHealthcheckURLFallsBackWhenAddressIsUnusable(t *testing.T) {
	cfg := Config{Server: Server{Addr: "nonsense"}}

	if got := cfg.HealthcheckURL(); got != "http://127.0.0.1:8080/api/v1/health" {
		t.Errorf("HealthcheckURL = %q, want the default fallback", got)
	}
}

func TestIsLoopback(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		"192.168.1.5:80": false,
		":8080":          false,
	}

	for addr, want := range tests {
		cfg := Config{Server: Server{Addr: addr}}
		if got := cfg.IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
