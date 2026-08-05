package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Image reference normalization tests.
//
// This is the SSRF gate: it is the only place a registry host is produced, so
// every refusal below is a security control rather than a parsing nicety. The
// refusal cases are therefore the longer half of this file, and each names what
// it is refusing and why.

func TestNormalizeImageRef(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		canonical string
		familiar  string
		kind      domain.RegistryKind
		host      string
		apiHost   string
		namespace string
		path      string
		tag       string
		digest    string
	}{
		{
			name:      "an official image gets the implicit library namespace",
			raw:       "nginx",
			canonical: "docker.io/library/nginx:latest",
			familiar:  "nginx:latest",
			kind:      domain.RegistryDockerHub,
			host:      "docker.io",
			apiHost:   "registry-1.docker.io",
			namespace: "library",
			path:      "library/nginx",
			tag:       "latest",
		},
		{
			name:      "a user image keeps its namespace",
			raw:       "grafana/grafana:11.0.0",
			canonical: "docker.io/grafana/grafana:11.0.0",
			familiar:  "grafana/grafana:11.0.0",
			kind:      domain.RegistryDockerHub,
			host:      "docker.io",
			apiHost:   "registry-1.docker.io",
			namespace: "grafana",
			path:      "grafana/grafana",
			tag:       "11.0.0",
		},
		{
			name:      "an explicit docker.io host normalises the same way",
			raw:       "docker.io/library/redis:7",
			canonical: "docker.io/library/redis:7",
			familiar:  "redis:7",
			kind:      domain.RegistryDockerHub,
			host:      "docker.io",
			apiHost:   "registry-1.docker.io",
			namespace: "library",
			path:      "library/redis",
			tag:       "7",
		},
		{
			name:      "the legacy index.docker.io spelling normalises too",
			raw:       "index.docker.io/library/redis:7",
			canonical: "docker.io/library/redis:7",
			familiar:  "redis:7",
			kind:      domain.RegistryDockerHub,
			host:      "docker.io",
			apiHost:   "registry-1.docker.io",
			namespace: "library",
			path:      "library/redis",
			tag:       "7",
		},
		{
			name:      "ghcr is recognised",
			raw:       "ghcr.io/owner/app:1.2.3",
			canonical: "ghcr.io/owner/app:1.2.3",
			familiar:  "ghcr.io/owner/app:1.2.3",
			kind:      domain.RegistryGHCR,
			host:      "ghcr.io",
			apiHost:   "ghcr.io",
			namespace: "owner",
			path:      "owner/app",
			tag:       "1.2.3",
		},
		{
			name:      "any other host is a generic OCI registry",
			raw:       "quay.io/prometheus/node-exporter:v1.8.1",
			canonical: "quay.io/prometheus/node-exporter:v1.8.1",
			familiar:  "quay.io/prometheus/node-exporter:v1.8.1",
			kind:      domain.RegistryOCI,
			host:      "quay.io",
			apiHost:   "quay.io",
			namespace: "prometheus",
			path:      "prometheus/node-exporter",
			tag:       "v1.8.1",
		},
		{
			name:      "a deep repository path keeps every segment",
			raw:       "registry.example.com/team/sub/app:2.0",
			canonical: "registry.example.com/team/sub/app:2.0",
			familiar:  "registry.example.com/team/sub/app:2.0",
			kind:      domain.RegistryOCI,
			host:      "registry.example.com",
			apiHost:   "registry.example.com",
			namespace: "team/sub",
			path:      "team/sub/app",
			tag:       "2.0",
		},
		{
			name:      "a digest-pinned reference keeps its digest",
			raw:       "nginx@sha256:" + strings.Repeat("a", 64),
			canonical: "docker.io/library/nginx@sha256:" + strings.Repeat("a", 64),
			familiar:  "nginx@sha256:" + strings.Repeat("a", 64),
			kind:      domain.RegistryDockerHub,
			host:      "docker.io",
			apiHost:   "registry-1.docker.io",
			namespace: "library",
			path:      "library/nginx",
			digest:    "sha256:" + strings.Repeat("a", 64),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := domain.NormalizeImageRef(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeImageRef(%q): %v", tc.raw, err)
			}

			if ref.Canonical != tc.canonical {
				t.Errorf("canonical = %q, want %q", ref.Canonical, tc.canonical)
			}
			if ref.Familiar != tc.familiar {
				t.Errorf("familiar = %q, want %q", ref.Familiar, tc.familiar)
			}
			if ref.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", ref.Kind, tc.kind)
			}
			if ref.Host != tc.host {
				t.Errorf("host = %q, want %q", ref.Host, tc.host)
			}
			if ref.APIHost != tc.apiHost {
				t.Errorf("apiHost = %q, want %q", ref.APIHost, tc.apiHost)
			}
			if ref.Namespace != tc.namespace {
				t.Errorf("namespace = %q, want %q", ref.Namespace, tc.namespace)
			}
			if ref.Path != tc.path {
				t.Errorf("path = %q, want %q", ref.Path, tc.path)
			}
			if ref.Tag != tc.tag {
				t.Errorf("tag = %q, want %q", ref.Tag, tc.tag)
			}
			if ref.Digest != tc.digest {
				t.Errorf("digest = %q, want %q", ref.Digest, tc.digest)
			}
			if ref.Pinned() != (tc.digest != "") {
				t.Errorf("pinned = %v", ref.Pinned())
			}
		})
	}
}

// Normalization must be IDEMPOTENT: the canonical form of a canonical form is
// itself. Without that the cache key would fragment and one image would be
// checked under two identities.
func TestNormalizationIsIdempotent(t *testing.T) {
	for _, raw := range []string{
		"nginx", "grafana/grafana:11.0.0", "ghcr.io/owner/app:1.2.3",
		"quay.io/prometheus/node-exporter:v1.8.1",
		"nginx@sha256:" + strings.Repeat("b", 64),
	} {
		first, err := domain.NormalizeImageRef(raw)
		if err != nil {
			t.Fatalf("NormalizeImageRef(%q): %v", raw, err)
		}
		second, err := domain.NormalizeImageRef(first.Canonical)
		if err != nil {
			t.Fatalf("re-normalising %q: %v", first.Canonical, err)
		}
		if second.Canonical != first.Canonical {
			t.Errorf("%q normalised to %q then %q", raw, first.Canonical, second.Canonical)
		}
	}
}

// THE SSRF GATE. Every case here is a reference that must never become a
// network destination, and each is a shape an attacker or a misconfiguration
// would actually produce.
func TestNormalizationRefusesEverythingThatIsNotAPublicRegistry(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		// Addresses. A registry named by address is indistinguishable from an
		// attempt to steer HarborMaster at an internal service.
		{"an IPv4 literal", "127.0.0.1/app:1"},
		{"a private IPv4 literal", "10.0.0.5/app:1"},
		{"a link-local IPv4 literal", "169.254.169.254/app:1"},
		{"a public IPv4 literal", "93.184.216.34/app:1"},
		{"an IPv6 literal", "[::1]/app:1"},

		// Local and internal names.
		{"localhost", "localhost/app:1"},
		{"localhost with a trailing dot", "localhost./app:1"},

		// Ports mean a non-default endpoint, which in practice means internal.
		{"a host with a port", "registry.example.com:5000/app:1"},
		{"localhost with a port", "localhost:5000/app:1"},

		// Shapes that exist to confuse a URL parser.
		{"userinfo in the host", "user:pass@registry.example.com/app:1"},
		{"a scheme", "https://registry.example.com/app:1"},
		{"a fragment", "registry.example.com#@evil.example.com/app:1"},
		{"a backslash", "registry.example.com\\@evil.example.com/app:1"},
		{"a double dot", "registry..example.com/app:1"},
		{"a leading dot", ".example.com/app:1"},
		{"a trailing dot", "example.com./app:1"},
		{"an underscore in the host", "reg_istry.example.com/app:1"},
		{"a percent escape in the host", "registry%2eexample.com/app:1"},

		// Path traversal in the repository, which would otherwise reach a URL.
		{"a traversal segment", "registry.example.com/../app:1"},
		{"a dot segment", "registry.example.com/./app:1"},
		{"a leading slash", "registry.example.com//app:1"},

		// Malformed or oversized input.
		{"empty", ""},
		{"whitespace", "   "},
		{"an oversized reference", strings.Repeat("a", domain.MaxReferenceBytes+1)},
		{"an oversized tag", "nginx:" + strings.Repeat("a", domain.MaxTagBytes+1)},
		{"a malformed digest", "nginx@sha256:tooshort"},
		{"an unknown digest algorithm", "nginx@md5:" + strings.Repeat("a", 32)},
		{"a non-hex digest", "nginx@sha256:" + strings.Repeat("z", 64)},
		{"an uppercase repository", "registry.example.com/App:1"},
		{"a tag starting with a dot", "nginx:.hidden"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := domain.NormalizeImageRef(tc.raw)
			if err == nil {
				t.Fatalf("accepted %q as %+v; it must never become a destination", tc.raw, ref)
			}
			if !errors.Is(err, domain.ErrUnsupportedReference) {
				t.Errorf("error = %v, want ErrUnsupportedReference", err)
			}
			// Nothing usable may be returned alongside the refusal: a caller
			// that ignored the error must still not get a host.
			if ref.APIHost != "" || ref.Host != "" {
				t.Errorf("a refused reference still carried a host: %+v", ref)
			}
		})
	}
}

// A single-label first component is a NAMESPACE, not a host.
//
// This is Docker's own rule and it is worth asserting rather than assuming:
// reading "registry/app" as the host "registry" would send a request to an
// internal name, and reading "grafana/grafana" as a host would break every user
// image. The dot-or-colon test is what separates the two, so both sides of it
// are pinned here.
func TestASingleLabelComponentIsANamespaceNotAHost(t *testing.T) {
	for _, raw := range []string{"registry/app:1", "grafana/grafana:1", "internal/thing:1"} {
		ref, err := domain.NormalizeImageRef(raw)
		if err != nil {
			t.Fatalf("NormalizeImageRef(%q): %v", raw, err)
		}
		if ref.Host != domain.DockerHubFamiliarHost {
			t.Errorf("%q resolved to host %q, want Docker Hub", raw, ref.Host)
		}
		if ref.APIHost != domain.DockerHubAPIHost {
			t.Errorf("%q resolved to api host %q", raw, ref.APIHost)
		}
	}

	// The same shape WITH a colon is read as a host, and then refused for
	// carrying a port. That is the branch the rule above must not swallow.
	if _, err := domain.NormalizeImageRef("registry:5000/app:1"); !errors.Is(err, domain.ErrUnsupportedReference) {
		t.Errorf("a host with a port returned %v, want ErrUnsupportedReference", err)
	}
}

// The realm host check shares this function, so its behaviour is asserted
// directly as well as through normalization.
func TestContactableRegistryHost(t *testing.T) {
	for _, host := range []string{
		"registry-1.docker.io", "ghcr.io", "quay.io", "registry.example.com",
		"a.b.c.d.example.com",
	} {
		if !domain.ContactableRegistryHost(host) {
			t.Errorf("ContactableRegistryHost(%q) = false, want true", host)
		}
	}

	for _, host := range []string{
		"", "localhost", "registry", "127.0.0.1", "::1", "10.0.0.1",
		"registry.example.com:443", "user@registry.example.com",
		"registry.example.com/path", "-leading.example.com",
		"trailing-.example.com", "reg istry.example.com",
		strings.Repeat("a", 254) + ".com",
		strings.Repeat("a", 64) + ".example.com",
	} {
		if domain.ContactableRegistryHost(host) {
			t.Errorf("ContactableRegistryHost(%q) = true, want false", host)
		}
	}
}

// Tags and digests also arrive FROM a registry, in a tag listing, so the
// validators are exported and tested as the untrusted-input filters they are.
func TestTagAndDigestValidation(t *testing.T) {
	for _, tag := range []string{"latest", "1.25.3", "v1.2.3-alpine", "a_b.c-d", "A1"} {
		if !domain.ValidImageTag(tag) {
			t.Errorf("ValidImageTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range []string{
		"", ".hidden", "-leading", "has space", "has/slash", "has:colon",
		"has\nnewline", strings.Repeat("a", domain.MaxTagBytes+1),
	} {
		if domain.ValidImageTag(tag) {
			t.Errorf("ValidImageTag(%q) = true, want false", tag)
		}
	}

	if !domain.ValidImageDigest("sha256:" + strings.Repeat("f", 64)) {
		t.Error("a well-formed sha256 digest was rejected")
	}
	if !domain.ValidImageDigest("sha512:" + strings.Repeat("0", 128)) {
		t.Error("a well-formed sha512 digest was rejected")
	}
	for _, digest := range []string{
		"", "sha256", "sha256:", "sha256:short", "md5:" + strings.Repeat("a", 32),
		"sha256:" + strings.Repeat("A", 64), // uppercase hex is not canonical
		"sha256:" + strings.Repeat("a", 63),
		"../../etc/passwd",
	} {
		if domain.ValidImageDigest(digest) {
			t.Errorf("ValidImageDigest(%q) = true, want false", digest)
		}
	}
}

// A reference that differs only in how it is spelled must produce ONE identity,
// or the same image would be looked up twice and counted twice.
func TestDuplicateReferenceSpellingsCollapse(t *testing.T) {
	spellings := []string{
		"nginx:1.25",
		"docker.io/library/nginx:1.25",
		"index.docker.io/library/nginx:1.25",
		"  nginx:1.25  ",
	}

	var canonical string
	for _, raw := range spellings {
		ref, err := domain.NormalizeImageRef(raw)
		if err != nil {
			t.Fatalf("NormalizeImageRef(%q): %v", raw, err)
		}
		if canonical == "" {
			canonical = ref.Canonical
			continue
		}
		if ref.Canonical != canonical {
			t.Errorf("%q normalised to %q, want %q", raw, ref.Canonical, canonical)
		}
	}
}
