package api

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// staticHandler serves the compiled single-page frontend from an fs.FS.
//
// Unknown paths fall back to index.html so client-side routes such as
// /containers survive a page reload. Requests under /api/ never reach here.
type staticHandler struct {
	assets  fs.FS
	logger  *slog.Logger
	fileSrv http.Handler
	// available is false when the binary was built without a frontend bundle.
	available bool
}

// newStaticHandler builds the SPA handler. A missing or empty asset tree is not
// an error: the binary stays usable as an API-only server and says so.
func newStaticHandler(assets fs.FS, logger *slog.Logger) *staticHandler {
	h := &staticHandler{assets: assets, logger: logger}
	if assets == nil {
		return h
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		logger.Warn("frontend bundle not embedded; serving API only. Run `make web-build` before `make build` to embed the UI.")
		return h
	}
	h.available = true
	h.fileSrv = http.FileServer(http.FS(assets))
	return h
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.available {
		writeError(w, r, h.logger, http.StatusNotFound, CodeNotFound,
			"frontend bundle not built into this binary")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, r, h.logger, http.StatusMethodNotAllowed, CodeMethodNotAllwd, "method not allowed")
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		h.serveIndex(w, r)
		return
	}

	// fs.Stat rejects traversal itself, but checking the cleaned name keeps the
	// intent explicit at the boundary.
	info, err := fs.Stat(h.assets, name)
	switch {
	case err == nil && !info.IsDir():
		// Hashed asset filenames are immutable; index.html must never be.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.fileSrv.ServeHTTP(w, r)
	case err == nil || errors.Is(err, fs.ErrNotExist):
		// A directory or a client-side route: hand the SPA its entry point.
		h.serveIndex(w, r)
	default:
		h.logger.ErrorContext(r.Context(), "read embedded asset failed",
			slog.String("path", name), slog.String("error", err.Error()))
		writeError(w, r, h.logger, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

func (h *staticHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(h.assets, "index.html")
	if err != nil {
		h.logger.ErrorContext(r.Context(), "read embedded index.html failed",
			slog.String("error", err.Error()))
		writeError(w, r, h.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	// gosec's taint analysis flags this as XSS (G705) because `index` reaches
	// the response body from a file read. It is a false positive, and the
	// reason is structural rather than a judgement call: the path is the
	// LITERAL "index.html", and h.assets is an embed.FS compiled into the
	// binary. The bytes are build output. No request, no Docker payload, and
	// no database row can influence what is read here or reach this response.
	//
	// The genuine XSS surface for a SPA is what the app renders at runtime,
	// and that is guarded separately: React escapes by default, nothing uses
	// dangerouslySetInnerHTML, and the CSP set by withSecurityHeaders forbids
	// inline script.
	//
	//nolint:gosec // G705: embedded build output at a literal path; no request data reaches it.
	if _, err := w.Write(index); err != nil {
		h.logger.DebugContext(r.Context(), "write index.html failed", slog.String("error", err.Error()))
	}
}
