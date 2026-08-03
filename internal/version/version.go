// Package version exposes build metadata for the HarborMaster binary.
//
// The values are overridden at link time, for example:
//
//	go build -ldflags "-X github.com/Aznyi/HarborMaster/internal/version.version=v0.1.0"
package version

import "runtime"

// Injected via -ldflags at build time; the defaults describe a local dev build.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// Info is the build metadata reported by the API.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build metadata of the running binary.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}
