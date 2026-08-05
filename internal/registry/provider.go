package registry

import (
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Provider describes how one family of registries behaves.
//
// # Why this interface is small
//
// The OCI distribution API is the same everywhere: the manifest path, the tag
// listing path, the media types, and the bearer-token challenge are all
// specified. A provider abstraction that re-stated any of that would be
// ceremony, and would be the "hardcoded registry behaviour spread through the
// codebase" it exists to prevent.
//
// So this interface carries only what genuinely DIFFERS between registries, and
// each method exists because a real registry needs it:
//
//   - Handles: which hosts a provider serves. Adding a registry is adding a
//     Provider and registering it; nothing else in the codebase changes.
//   - Scope: the authorization scope for an anonymous pull token. Universal
//     today, but it is the field a registry with a different scope grammar
//     would need.
//   - TagPageSize: registries disagree about how many tags they will return
//     per page, and some silently cap a larger request.
//   - SupportsTagListing: not every registry exposes tag enumeration. One that
//     does not must be reported as "cannot determine" rather than "no update".
//
// Everything else -- the host, the repository path, the scheme -- is decided by
// domain.NormalizeImageRef, which is deliberately the single source of network
// destinations.
type Provider interface {
	// Kind identifies the provider in stored records and in the API.
	Kind() domain.RegistryKind
	// Handles reports whether this provider serves the given API host.
	Handles(apiHost string) bool
	// Scope returns the authorization scope for a PULL-ONLY token.
	//
	// Pull-only is not a detail: HarborMaster must never hold a token that
	// could push or delete, so the scope it asks for is the narrowest one that
	// answers the question.
	Scope(repository string) string
	// TagPageSize is how many tags to request per page.
	TagPageSize() int
	// SupportsTagListing reports whether tag enumeration is available.
	SupportsTagListing() bool
}

// pullScope renders the standard pull-only scope.
//
// Shared by every provider today. It is a function rather than a default method
// because Go interfaces have no defaults, and duplicating the string in each
// provider is how the two would eventually disagree.
func pullScope(repository string) string {
	return "repository:" + repository + ":pull"
}

// dockerHubProvider serves Docker Hub.
type dockerHubProvider struct{}

func (dockerHubProvider) Kind() domain.RegistryKind { return domain.RegistryDockerHub }

func (dockerHubProvider) Handles(apiHost string) bool {
	return apiHost == domain.DockerHubAPIHost
}

func (dockerHubProvider) Scope(repository string) string { return pullScope(repository) }

// TagPageSize is 100 because Docker Hub caps a larger request there anyway, and
// asking for what will be granted keeps the page count honest.
func (dockerHubProvider) TagPageSize() int { return 100 }

func (dockerHubProvider) SupportsTagListing() bool { return true }

// ghcrProvider serves the GitHub Container Registry.
type ghcrProvider struct{}

func (ghcrProvider) Kind() domain.RegistryKind { return domain.RegistryGHCR }

func (ghcrProvider) Handles(apiHost string) bool { return apiHost == domain.GHCRHost }

func (ghcrProvider) Scope(repository string) string { return pullScope(repository) }

func (ghcrProvider) TagPageSize() int { return 100 }

func (ghcrProvider) SupportsTagListing() bool { return true }

// ociProvider serves any other registry speaking the distribution API.
//
// The fallback, and the reason the set below is ordered: it Handles every host,
// so it must be consulted last.
type ociProvider struct{}

func (ociProvider) Kind() domain.RegistryKind { return domain.RegistryOCI }

func (ociProvider) Handles(string) bool { return true }

func (ociProvider) Scope(repository string) string { return pullScope(repository) }

// TagPageSize is conservative for an unknown registry: a smaller page is more
// widely accepted, and the cost is one extra round trip on a large repository.
func (ociProvider) TagPageSize() int { return 50 }

func (ociProvider) SupportsTagListing() bool { return true }

// providers is the ordered provider set.
//
// Ordered because ociProvider matches everything. A provider added here must go
// BEFORE it, and nothing else in the codebase needs to change.
var providers = []Provider{
	dockerHubProvider{},
	ghcrProvider{},
	ociProvider{},
}

// providerFor returns the provider serving an API host.
//
// Never nil: ociProvider terminates the search.
func providerFor(apiHost string) Provider {
	host := strings.ToLower(strings.TrimSpace(apiHost))
	for _, provider := range providers {
		if provider.Handles(host) {
			return provider
		}
	}
	return ociProvider{}
}

// ProviderKind reports which provider serves a host, for display and storage.
func ProviderKind(apiHost string) domain.RegistryKind {
	return providerFor(apiHost).Kind()
}
