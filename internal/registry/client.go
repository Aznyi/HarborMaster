package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The OCI distribution client.
//
// Two operations, both reads: resolve a reference to a manifest digest, and
// list a repository's tags. There is no third operation, and no code path that
// sends anything but GET.

// Response budgets.
//
// A registry is a third party, so every body is bounded before it is read. The
// numbers are generous for real content and small enough that a hostile
// registry cannot make HarborMaster allocate: a manifest index for fifteen
// platforms is a few kilobytes.
const (
	maxManifestBytes = 4 << 20  // 4 MiB
	maxTagsBytes     = 4 << 20  // 4 MiB
	maxTokenBytes    = 64 << 10 // 64 KiB
	// maxIndexEntries bounds how many platform entries are read from an index.
	maxIndexEntries = 128
	// maxTagsPerPage bounds how many tags are accepted from one page, whatever
	// the registry actually sends.
	maxTagsPerPage = 1024
	// maxTrackedTags bounds how many tags are held in memory across all pages
	// of one listing.
	maxTrackedTags = 4096
	// maxAnnotationBytes bounds one stored annotation value.
	maxAnnotationBytes = 512
	// maxAnnotations bounds how many annotations are kept.
	maxAnnotations = 16
)

// Manifest media types, in Accept order.
//
// Indexes are listed first so a multi-architecture image resolves to its INDEX
// digest -- which is what the local daemon records in RepoDigests, and therefore
// the only digest that compares meaningfully against it.
const acceptManifests = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// annotationKeys is the allowlist of OCI annotations that are kept.
//
// An allowlist because annotations are publisher-controlled and unbounded in
// both count and size. Everything outside this set is discarded rather than
// stored.
var annotationKeys = map[string]string{
	"org.opencontainers.image.created":       "created",
	"org.opencontainers.image.vendor":        "vendor",
	"org.opencontainers.image.source":        "source",
	"org.opencontainers.image.revision":      "revision",
	"org.opencontainers.image.url":           "url",
	"org.opencontainers.image.title":         "title",
	"org.opencontainers.image.description":   "description",
	"org.opencontainers.image.licenses":      "licenses",
	"org.opencontainers.image.version":       "version",
	"org.opencontainers.image.documentation": "documentation",
}

// Options configures a Client.
type Options struct {
	// Version identifies HarborMaster in the User-Agent.
	Version string
	// RequestTimeout bounds one HTTP request, including its retries.
	RequestTimeout time.Duration
	// MaxAttempts bounds how many times one request is tried. 1 disables
	// retrying.
	MaxAttempts int
	// RetryBackoff is the base delay between attempts, doubled each time.
	RetryBackoff time.Duration
	// Now is injectable so retry and expiry behaviour is deterministic in
	// tests.
	Now func() time.Time
}

// Client reads manifests and tag listings from OCI registries.
//
// Safe for concurrent use. Concurrency is bounded by the CALLER, which owns the
// worker pool: a client that bounded it internally would hide the limit from
// the place that has to reason about registry politeness.
type Client struct {
	http      *http.Client
	userAgent string

	requestTimeout time.Duration
	maxAttempts    int
	retryBackoff   time.Duration
	now            func() time.Time

	// tokens caches anonymous pull tokens by host and scope.
	//
	// IN MEMORY ONLY. A token is a bearer credential; it is never written to
	// the database, never logged, and does not survive a restart. The cache is
	// bounded so a large estate cannot grow it without limit.
	tokens sync.Map
	// tokenCount tracks the cache size for the bound above.
	tokenCount   int
	tokenCountMu sync.Mutex
}

// maxCachedTokens bounds the in-memory token cache.
const maxCachedTokens = 512

// cachedToken is one anonymous pull token.
type cachedToken struct {
	value     string
	expiresAt time.Time
}

// New builds a Client.
func New(opts Options) *Client {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	attempts := opts.MaxAttempts
	if attempts < 1 {
		attempts = 3
	}
	backoff := opts.RetryBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	return &Client{
		http:           newHTTPClient(),
		userAgent:      UserAgent(opts.Version),
		requestTimeout: timeout,
		maxAttempts:    attempts,
		retryBackoff:   backoff,
		now:            now,
	}
}

// rawResponse is what a completed request yields.
//
// Deliberately NOT an *http.Response. By the time do returns, the body has been
// read to its budget and closed, so handing back the response object would offer
// callers a Body they must not touch -- and would leave a static analyser
// correctly unable to prove the close ever happened.
type rawResponse struct {
	StatusCode int
	Header     http.Header
}

// ManifestRequest asks for one reference's manifest.
type ManifestRequest struct {
	Ref domain.NormalizedRef
	// ETag from a previous response, for a conditional request. Empty skips
	// the conditional.
	ETag string
}

// ManifestResult is what a manifest lookup established.
type ManifestResult struct {
	// NotModified reports that the registry answered 304: the cached digest is
	// still current and no body was transferred.
	NotModified bool

	// Digest is COMPUTED from the response body rather than read from the
	// Docker-Content-Digest header. The digest of a manifest is by definition
	// the hash of its bytes, so computing it is both cheaper to trust and
	// strictly more correct than believing a header.
	Digest    string
	MediaType string
	// ETag, when the registry supplied one, for the next conditional request.
	ETag string
	// Size is the manifest document's size in bytes.
	Size int64

	// Platforms lists the platforms an index advertises. Empty for a
	// single-platform manifest.
	Platforms []domain.Platform
	// Annotations holds the allowlisted OCI annotations, already bounded and
	// sanitised.
	Annotations map[string]string
}

// Manifest resolves a reference to its manifest digest.
//
// One request. The client deliberately does NOT descend into an index to fetch
// a platform-specific manifest or a config blob:
//
//   - The index digest is what the local daemon records, so it is the digest
//     that compares correctly.
//   - Blob endpoints redirect, and this package refuses redirects.
//   - A config blob is unbounded publisher content for two fields that OCI
//     annotations already carry when a publisher cares to set them.
func (c *Client) Manifest(ctx context.Context, req ManifestRequest) (ManifestResult, error) {
	reference := req.Ref.Tag
	if req.Ref.Pinned() {
		reference = req.Ref.Digest
	}
	if reference == "" {
		return ManifestResult{}, domain.ErrUnsupportedReference
	}

	endpoint, err := manifestURL(req.Ref, reference)
	if err != nil {
		return ManifestResult{}, err
	}

	provider := providerFor(req.Ref.APIHost)
	header := http.Header{"Accept": []string{acceptManifests}}
	if req.ETag != "" && validETag(req.ETag) {
		header.Set("If-None-Match", req.ETag)
	}

	response, body, err := c.get(ctx, endpoint, header, provider, req.Ref.Path, maxManifestBytes)
	if err != nil {
		return ManifestResult{}, err
	}

	if response.StatusCode == http.StatusNotModified {
		return ManifestResult{NotModified: true, ETag: sanitiseETag(response.Header.Get("ETag"))}, nil
	}

	sum := sha256.Sum256(body)
	result := ManifestResult{
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		MediaType: sanitiseMediaType(response.Header.Get("Content-Type")),
		ETag:      sanitiseETag(response.Header.Get("ETag")),
		Size:      int64(len(body)),
	}

	document, err := decodeManifest(body)
	if err != nil {
		return ManifestResult{}, err
	}
	if document.MediaType != "" && result.MediaType == "" {
		result.MediaType = sanitiseMediaType(document.MediaType)
	}
	result.Platforms = document.platforms()
	result.Annotations = document.annotations()

	return result, nil
}

// TagsResult is a repository's tag listing.
type TagsResult struct {
	// Tags holds every well-formed tag read. Malformed entries are discarded:
	// a tag from a registry is untrusted input that would otherwise reach a URL
	// and a UI.
	Tags []string
	// Truncated reports that the listing hit its page or size budget, so the
	// set is incomplete. The caller MUST report an incomplete listing as
	// "unknown" rather than "no update available".
	Truncated bool
}

// Tags lists a repository's tags.
//
// Paginated with the specification's own `last` cursor, built from the last tag
// HarborMaster itself received -- deliberately NOT by following the Link header
// the registry returns. A Link header is a registry-supplied URL, which is the
// one input this package refuses to accept.
//
// maxPages bounds the walk. A repository with more tags than the budget yields
// Truncated, which the caller turns into an honest "cannot determine" rather
// than a false "up to date".
func (c *Client) Tags(ctx context.Context, ref domain.NormalizedRef, maxPages int) (TagsResult, error) {
	provider := providerFor(ref.APIHost)
	if !provider.SupportsTagListing() {
		return TagsResult{}, ErrTagListingUnsupported
	}
	if maxPages < 1 {
		maxPages = 1
	}

	var (
		result TagsResult
		last   string
	)

	for page := 0; page < maxPages; page++ {
		endpoint, err := tagsURL(ref, provider.TagPageSize(), last)
		if err != nil {
			return TagsResult{}, err
		}

		header := http.Header{"Accept": []string{"application/json"}}
		_, body, err := c.get(ctx, endpoint, header, provider, ref.Path, maxTagsBytes)
		if err != nil {
			// A registry that does not implement listing answers 404 on this
			// path even though the repository exists. Reported as unsupported
			// so the caller falls back to digest comparison rather than
			// recording the image as missing.
			if errors.Is(err, ErrNotFound) && page == 0 {
				return TagsResult{}, ErrTagListingUnsupported
			}
			return TagsResult{}, err
		}

		var document struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &document); err != nil {
			return TagsResult{}, fmt.Errorf("%w: tag listing", ErrMalformedResponse)
		}

		accepted := 0
		for index, tag := range document.Tags {
			if index >= maxTagsPerPage {
				result.Truncated = true
				break
			}
			if !domain.ValidImageTag(tag) {
				continue
			}
			if len(result.Tags) >= maxTrackedTags {
				result.Truncated = true
				break
			}
			result.Tags = append(result.Tags, tag)
			accepted++
			last = tag
		}

		if result.Truncated {
			return result, nil
		}
		// A short page means the listing is exhausted. A page that returned
		// nothing usable also ends the walk, or a registry returning only
		// malformed tags would spin the loop to its page budget.
		if len(document.Tags) < provider.TagPageSize() || accepted == 0 {
			return result, nil
		}
		if page == maxPages-1 {
			result.Truncated = true
		}
	}
	return result, nil
}

// ------------------------------------------------------------- requesting --

// get performs a bounded GET with retries and one anonymous-token negotiation.
func (c *Client) get(
	ctx context.Context,
	endpoint *url.URL,
	header http.Header,
	provider Provider,
	repository string,
	limit int64,
) (rawResponse, []byte, error) {
	var lastErr error

	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff between attempts, cancellable. A fixed sleep
			// would keep a shutdown waiting on a registry.
			delay := c.retryBackoff << (attempt - 1)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return rawResponse{}, nil, ctx.Err()
			case <-timer.C:
			}
		}

		response, body, err := c.attempt(ctx, endpoint, header, provider, repository, limit)
		if err == nil {
			return response, body, nil
		}
		lastErr = err

		// Only transient failures are retried. A 404, a 401, or a rate limit
		// would return the same answer and retrying a rate limit is precisely
		// what the registry asked HarborMaster not to do.
		if !Transient(err) {
			return rawResponse{}, nil, err
		}
		if ctx.Err() != nil {
			return rawResponse{}, nil, ctx.Err()
		}
	}
	return rawResponse{}, nil, lastErr
}

// attempt performs one request, negotiating a token if challenged.
func (c *Client) attempt(
	ctx context.Context,
	endpoint *url.URL,
	header http.Header,
	provider Provider,
	repository string,
	limit int64,
) (rawResponse, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	response, body, err := c.do(requestCtx, endpoint, header, "", limit)
	if err != nil {
		return rawResponse{}, nil, err
	}

	// The distribution API answers an anonymous request for a public repository
	// with a 401 carrying a bearer challenge. Negotiating once and retrying is
	// the specified flow.
	if response.StatusCode == http.StatusUnauthorized {
		challenge, ok := parseChallenge(response.Header.Get("WWW-Authenticate"))
		if !ok {
			return rawResponse{}, nil, ErrUnauthorized
		}

		token, tokenErr := c.token(requestCtx, challenge, provider, repository)
		if tokenErr != nil {
			return rawResponse{}, nil, tokenErr
		}

		response, body, err = c.do(requestCtx, endpoint, header, token, limit)
		if err != nil {
			return rawResponse{}, nil, err
		}
	}

	if response.StatusCode == http.StatusNotModified {
		return response, nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return rawResponse{}, nil, statusError(response.StatusCode, response.Header, c.now())
	}
	return response, body, nil
}

// do issues one request and reads a bounded body.
func (c *Client) do(
	ctx context.Context,
	endpoint *url.URL,
	header http.Header,
	token string,
	limit int64,
) (rawResponse, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return rawResponse{}, nil, fmt.Errorf("%w: building request", ErrPermanent)
	}

	for key, values := range header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Set("User-Agent", c.userAgent)
	if token != "" {
		// The only place an Authorization header is ever set. It is never
		// logged and never stored.
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		// A redirect refusal arrives wrapped in a *url.Error; unwrapping keeps
		// the sentinel usable by Classify.
		if errors.Is(err, ErrRedirectRefused) {
			return rawResponse{}, nil, ErrRedirectRefused
		}
		if errors.Is(err, ErrBlockedAddress) {
			return rawResponse{}, nil, ErrBlockedAddress
		}
		return rawResponse{}, nil, err
	}
	// Every exit below this point has closed the body, which is what lets the
	// function hand back a value type rather than a live response.
	defer func() {
		// Drain a bounded amount so the connection can be reused, then close.
		// Draining without a bound would let a hostile registry stream forever
		// into a discard.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()

	summary := rawResponse{StatusCode: response.StatusCode, Header: response.Header}
	if response.StatusCode == http.StatusNotModified {
		return summary, nil, nil
	}

	// LimitReader with one extra byte, so exceeding the budget is DETECTED
	// rather than silently truncating a document into something that parses as
	// different content.
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return rawResponse{}, nil, err
	}
	if int64(len(body)) > limit {
		return rawResponse{}, nil, ErrResponseTooLarge
	}
	return summary, body, nil
}

// ----------------------------------------------------------------- tokens --

// challenge is a parsed WWW-Authenticate bearer challenge.
type challenge struct {
	realm   string
	service string
}

// parseChallenge reads a bearer challenge.
//
// Only realm and service are extracted; the scope is built by HarborMaster from
// the repository it asked about, never taken from the challenge. A
// registry-supplied scope could ask for push or delete rights, and there is no
// reason to let a server choose what a client requests.
func parseChallenge(value string) (challenge, bool) {
	const prefix = "Bearer "
	if len(value) > 2048 || !strings.HasPrefix(value, prefix) {
		return challenge{}, false
	}

	parsed := challenge{}
	for _, part := range splitChallengeParts(value[len(prefix):]) {
		key, raw, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		raw = strings.Trim(strings.TrimSpace(raw), `"`)

		switch key {
		case "realm":
			parsed.realm = raw
		case "service":
			parsed.service = raw
		}
	}
	return parsed, parsed.realm != ""
}

// splitChallengeParts splits a challenge on commas that are not inside quotes.
func splitChallengeParts(value string) []string {
	parts := make([]string, 0, 4)
	quoted := false
	start := 0

	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, value[start:index])
				start = index + 1
			}
		}
	}
	return append(parts, value[start:])
}

// token obtains an anonymous pull token, caching it in memory.
func (c *Client) token(ctx context.Context, challenge challenge, provider Provider, repository string) (string, error) {
	scope := provider.Scope(repository)
	key := challenge.realm + "\x1f" + challenge.service + "\x1f" + scope

	if cached, ok := c.tokens.Load(key); ok {
		token, valid := cached.(cachedToken)
		// A minute of headroom, so a token is not used at the instant it
		// expires.
		if valid && c.now().Add(time.Minute).Before(token.expiresAt) {
			return token.value, nil
		}
		c.tokens.Delete(key)
	}

	endpoint, err := tokenURL(challenge, scope)
	if err != nil {
		return "", err
	}

	header := http.Header{"Accept": []string{"application/json"}}
	_, body, err := c.do(ctx, endpoint, header, "", maxTokenBytes)
	if err != nil {
		return "", err
	}

	var document struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("%w: token response", ErrMalformedResponse)
	}

	token := document.Token
	if token == "" {
		token = document.AccessToken
	}
	if token == "" {
		return "", ErrUnauthorized
	}

	lifetime := time.Duration(document.ExpiresIn) * time.Second
	// A registry that omits or overstates the lifetime gets a conservative
	// default; a long-lived credential in memory is not something to accept on
	// a server's say-so.
	if lifetime <= 0 || lifetime > time.Hour {
		lifetime = 5 * time.Minute
	}
	c.storeToken(key, cachedToken{value: token, expiresAt: c.now().Add(lifetime)})

	return token, nil
}

// storeToken caches a token, keeping the cache bounded.
//
// At the bound the cache is cleared rather than evicted selectively: tokens are
// cheap to re-obtain, and an eviction policy would be more machinery than the
// problem deserves.
func (c *Client) storeToken(key string, token cachedToken) {
	c.tokenCountMu.Lock()
	defer c.tokenCountMu.Unlock()

	if c.tokenCount >= maxCachedTokens {
		c.tokens.Range(func(k, _ any) bool {
			c.tokens.Delete(k)
			return true
		})
		c.tokenCount = 0
	}
	if _, existed := c.tokens.Swap(key, token); !existed {
		c.tokenCount++
	}
}

// ------------------------------------------------------------------- URLs --

// manifestURL builds the manifest endpoint.
//
// Every component is validated before it arrives: the host by
// domain.NormalizeImageRef, the repository path and the reference by the same.
// The URL is ASSEMBLED from parts rather than formatted from a string, and the
// path is set through URL.Path so the encoder escapes anything unexpected.
func manifestURL(ref domain.NormalizedRef, reference string) (*url.URL, error) {
	if ref.APIHost == "" || ref.Path == "" {
		return nil, domain.ErrUnsupportedReference
	}
	return &url.URL{
		Scheme: "https",
		Host:   ref.APIHost,
		Path:   "/v2/" + ref.Path + "/manifests/" + reference,
	}, nil
}

// tagsURL builds the tag-listing endpoint with a self-constructed cursor.
func tagsURL(ref domain.NormalizedRef, pageSize int, last string) (*url.URL, error) {
	if ref.APIHost == "" || ref.Path == "" {
		return nil, domain.ErrUnsupportedReference
	}

	query := url.Values{}
	query.Set("n", fmt.Sprintf("%d", pageSize))
	if last != "" {
		query.Set("last", last)
	}

	return &url.URL{
		Scheme:   "https",
		Host:     ref.APIHost,
		Path:     "/v2/" + ref.Path + "/tags/list",
		RawQuery: query.Encode(),
	}, nil
}

// tokenURL builds the token endpoint from a challenge realm.
//
// THE REALM IS REGISTRY-SUPPLIED, so it is the one URL in this package that
// comes from a response. It is therefore validated exactly as strictly as an
// image reference is:
//
//   - it must parse, and be absolute
//   - the scheme must be https, never http
//   - the host must pass domain's own host rules, which refuse IP literals,
//     ports, localhost and single-label names
//   - userinfo is refused outright: a realm carrying credentials is either a
//     mistake or an attempt to make HarborMaster send something
//   - any query or fragment the realm carried is DISCARDED and replaced with
//     the parameters HarborMaster chose
//
// The dial-time address guard applies to the result as well, so even a realm
// that passes all of this cannot reach a private address.
func tokenURL(challenge challenge, scope string) (*url.URL, error) {
	if len(challenge.realm) > 512 {
		return nil, fmt.Errorf("%w: oversized realm", ErrMalformedResponse)
	}

	parsed, err := url.Parse(challenge.realm)
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable realm", ErrMalformedResponse)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: realm is not https", ErrMalformedResponse)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: realm carries userinfo", ErrMalformedResponse)
	}
	if !domain.ContactableRegistryHost(parsed.Host) {
		return nil, fmt.Errorf("%w: realm host is not contactable", ErrBlockedAddress)
	}

	query := url.Values{}
	if challenge.service != "" {
		if len(challenge.service) > 255 {
			return nil, fmt.Errorf("%w: oversized service", ErrMalformedResponse)
		}
		query.Set("service", challenge.service)
	}
	query.Set("scope", scope)

	return &url.URL{
		Scheme: "https",
		Host:   parsed.Host,
		Path:   parsed.Path,
		// The realm's own query and fragment are deliberately dropped.
		RawQuery: query.Encode(),
	}, nil
}

// --------------------------------------------------------------- decoding --

// manifestDocument is the subset of a manifest or index HarborMaster reads.
type manifestDocument struct {
	MediaType   string            `json:"mediaType"`
	Annotations map[string]string `json:"annotations"`
	Manifests   []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
		Platform    *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// decodeManifest parses a manifest body.
//
// Unknown fields are ignored rather than rejected: registries and tooling add
// fields, and a strict decode would break on a legitimate manifest. What
// protects the process is the size bound applied before this point, not the
// decoder's strictness.
func decodeManifest(body []byte) (manifestDocument, error) {
	var document manifestDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return manifestDocument{}, fmt.Errorf("%w: manifest", ErrMalformedResponse)
	}
	return document, nil
}

// platforms extracts the platforms an index advertises, bounded.
func (d manifestDocument) platforms() []domain.Platform {
	if len(d.Manifests) == 0 {
		return nil
	}

	limit := len(d.Manifests)
	if limit > maxIndexEntries {
		limit = maxIndexEntries
	}

	found := make([]domain.Platform, 0, limit)
	for index := 0; index < limit; index++ {
		entry := d.Manifests[index]
		if entry.Platform == nil {
			continue
		}
		// "unknown/unknown" is how buildx marks attestation manifests. They are
		// not platforms a container runs on.
		if entry.Platform.OS == "unknown" || entry.Platform.Architecture == "unknown" {
			continue
		}
		platform := domain.Platform{
			OS:           sanitiseShort(entry.Platform.OS, 32),
			Architecture: sanitiseShort(entry.Platform.Architecture, 32),
			Variant:      sanitiseShort(entry.Platform.Variant, 32),
		}
		if !platform.Empty() {
			found = append(found, platform)
		}
	}
	return found
}

// annotations extracts the allowlisted OCI annotations, bounded and sanitised.
//
// Index-level annotations are read first, then the first manifest entry's, so a
// single-platform image built by buildx still yields its provenance.
func (d manifestDocument) annotations() map[string]string {
	kept := make(map[string]string, maxAnnotations)

	collect := func(source map[string]string) {
		for key, value := range source {
			if len(kept) >= maxAnnotations {
				return
			}
			short, allowed := annotationKeys[key]
			if !allowed {
				continue
			}
			if _, already := kept[short]; already {
				continue
			}
			if clean := sanitiseAnnotation(value); clean != "" {
				kept[short] = clean
			}
		}
	}

	collect(d.Annotations)
	for index := 0; index < len(d.Manifests) && index < maxIndexEntries; index++ {
		collect(d.Manifests[index].Annotations)
	}

	if len(kept) == 0 {
		return nil
	}
	return kept
}

// --------------------------------------------------------------- cleaning --

// sanitiseAnnotation bounds and cleans a publisher-supplied annotation value.
//
// Invalid UTF-8 is refused rather than replaced, and any control character
// discards the whole value. An annotation reaches the database, the API, and a
// UI; a value carrying a newline is how a log line gets forged and how a
// terminal gets confused, and no legitimate annotation needs one.
func sanitiseAnnotation(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > maxAnnotationBytes {
		return ""
	}
	if !utf8.ValidString(trimmed) {
		return ""
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return trimmed
}

// sanitiseShort bounds and cleans a short identifier such as an architecture.
//
// An allowlist, because these values are compared and displayed and have no
// business containing anything else.
func sanitiseShort(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > limit {
		return ""
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return ""
		}
	}
	return trimmed
}

// sanitiseMediaType bounds and cleans a Content-Type.
//
// Parameters such as "; charset=utf-8" are dropped, so the stored value is the
// media type alone.
func sanitiseMediaType(value string) string {
	media, _, _ := strings.Cut(value, ";")
	trimmed := strings.TrimSpace(media)
	if trimmed == "" || len(trimmed) > 128 {
		return ""
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '/', r == '.', r == '+', r == '-', r == '_':
		default:
			return ""
		}
	}
	return trimmed
}

// validETag reports whether a cached ETag is safe to send back.
//
// An ETag round-trips through the database, so it is checked on the way OUT as
// well as on the way in: a header value assembled from a stored string is a
// header-injection gap if the string was never constrained.
func validETag(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

// sanitiseETag bounds and cleans a registry-supplied ETag before it is stored.
func sanitiseETag(value string) string {
	trimmed := strings.TrimSpace(value)
	if !validETag(trimmed) {
		return ""
	}
	return trimmed
}
