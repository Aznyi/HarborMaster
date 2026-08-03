// Package web embeds the compiled frontend bundle into the Go binary.
//
// `make web-build` populates web/dist; only .gitkeep is committed. The `all:`
// prefix is required so files Vite emits with a leading dot or underscore are
// included.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Assets returns the frontend bundle rooted at dist.
//
// It returns a usable FS even when the bundle is absent -- the API-only build
// is a supported mode, and the static handler reports it.
func Assets() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
