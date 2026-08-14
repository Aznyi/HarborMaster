module github.com/Aznyi/HarborMaster

// The LANGUAGE version the code is written against. Left at 1.25 deliberately:
// nothing here needs newer semantics, and raising it would drop support for
// building on the 1.25 line for no gain.
go 1.25.0

// The minimum TOOLCHAIN a build may use, which is a security floor rather than
// a language choice.
//
// The two are different questions. `go` above says which semantics the source
// relies on; this says which compiler is new enough to be safe to ship, because
// a Go binary carries the standard library it was built with. Building this
// module with 1.26.5 produced eight stdlib advisories in the released image
// (CVE-2026-46600 and CVE-2026-39821 at HIGH, plus -56862, -56860, -56859,
// -56858, -56853 and -33818), all fixed in 1.26.6.
//
// It also removes the analysis/build skew: actions/setup-go reads this
// directive from go.mod, so the CodeQL job that resolves its version from this
// file now analyses the same Go line the release is compiled with instead of
// the older one the `go` directive alone selected.
//
// Raise this together with GO_IMAGE in the Dockerfile.
toolchain go1.26.6

require (
	github.com/containerd/errdefs v1.0.0
	github.com/moby/docker-image-spec v1.3.1
	github.com/moby/moby/api v1.55.0
	github.com/moby/moby/client v0.5.1
	github.com/opencontainers/image-spec v1.1.1
	golang.org/x/crypto v0.54.0
	golang.org/x/term v0.45.0
	modernc.org/sqlite v1.56.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.8.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
