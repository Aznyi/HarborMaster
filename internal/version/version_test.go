package version_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/version"
)

func TestGetIsAlwaysPopulated(t *testing.T) {
	info := version.Get()

	// Defaults stand in for the ldflags values so an un-stamped dev build still
	// answers /api/v1/version with a complete payload.
	fields := map[string]string{
		"Version":   info.Version,
		"Commit":    info.Commit,
		"BuildDate": info.BuildDate,
		"GoVersion": info.GoVersion,
		"Platform":  info.Platform,
	}
	for name, value := range fields {
		if value == "" {
			t.Errorf("%s is empty", name)
		}
	}

	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; info.Platform != want {
		t.Errorf("Platform = %q, want %q", info.Platform, want)
	}
	if !strings.Contains(info.Platform, "/") {
		t.Errorf("Platform = %q, want os/arch", info.Platform)
	}
}
