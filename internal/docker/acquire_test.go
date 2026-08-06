package docker

import (
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Pull target and progress tests.
//
// These run INSIDE the package deliberately: the progress tracker is the thing
// that bounds a hostile registry's influence on HarborMaster's memory and
// database, and testing it through the Docker client would mean needing a
// daemon to test a pure function.
//
// The properties under test:
//
//   - A target that is not one immutable image is REFUSED before the daemon is
//     contacted. There is no such thing as a safe tag-only pull.
//   - Registry-supplied progress text is bounded, stripped, and rate-limited
//     before it can reach a column or a browser.

// ------------------------------------------------------------- targets --

const testDigest = "sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func validTarget() PullTarget {
	return PullTarget{
		Registry:   "docker.io",
		Repository: "library/nginx",
		Digest:     testDigest,
		Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
	}
}

// The reference sent to the daemon is always digest-pinned. This is the whole
// of "no mutable-tag-only execution": there is no branch that produces a tag.
func TestTheReferenceIsAlwaysDigestPinned(t *testing.T) {
	reference := validTarget().Reference()

	if !strings.Contains(reference, "@sha256:") {
		t.Fatalf("reference %q is not digest-pinned", reference)
	}
	if strings.Contains(reference, ":latest") || strings.Count(reference, ":") != 1 {
		t.Errorf("reference %q looks like it carries a tag", reference)
	}
	if reference != "docker.io/library/nginx@"+testDigest {
		t.Errorf("reference = %q", reference)
	}
}

// A target without a digest cannot be expressed as something the daemon will
// accept. This is the single most important refusal in the feature: without a
// digest, the content can change between approval and download.
func TestATargetWithoutADigestIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*PullTarget){
		"no digest":        func(target *PullTarget) { target.Digest = "" },
		"a tag instead":    func(target *PullTarget) { target.Digest = "1.27.1" },
		"truncated digest": func(target *PullTarget) { target.Digest = "sha256:abcdef" },
		"wrong algorithm":  func(target *PullTarget) { target.Digest = "md5:" + strings.Repeat("a", 32) },
		"digest with path": func(target *PullTarget) { target.Digest = testDigest + "/../other" },
	} {
		t.Run(name, func(t *testing.T) {
			target := validTarget()
			mutate(&target)

			if err := target.Validate(); err == nil {
				t.Fatalf("target %+v should be refused", target)
			}
		})
	}
}

// The registry host goes through the same SSRF gate an image reference does.
// The DAEMON performs the transfer, so this is not HarborMaster's own egress --
// but a host HarborMaster would never contact itself is not one it should ask
// the daemon to contact either.
func TestAnUncontactableRegistryIsRefused(t *testing.T) {
	for _, host := range []string{
		"", "localhost", "127.0.0.1", "registry.local:5000", "[::1]",
		"user@evil.example", "evil.example/path", "registry",
		strings.Repeat("a", 300) + ".example",
	} {
		target := validTarget()
		target.Registry = host

		if err := target.Validate(); err == nil {
			t.Errorf("registry %q should be refused", host)
		}
	}
}

// A repository path is an allowlist, because the value is about to be parsed by
// someone else's reference parser and then placed in a URL.
func TestAHostileRepositoryPathIsRefused(t *testing.T) {
	for _, path := range []string{
		"", "/library/nginx", "library/nginx/", "library//nginx",
		"library/../../etc/passwd", "library/nginx@" + testDigest,
		"library/nginx:latest", "library/nginx?query=1", "library/ nginx",
		"Library/Nginx", "library/nginx\n", "library/nginx#fragment",
		strings.Repeat("a", 300),
	} {
		target := validTarget()
		target.Repository = path

		if err := target.Validate(); err == nil {
			t.Errorf("repository %q should be refused", path)
		}
	}

	// And the ordinary shapes are accepted, so the allowlist is not simply
	// refusing everything.
	for _, path := range []string{
		"library/nginx", "nginx", "org/team/service", "my-app_1.0/sub",
	} {
		target := validTarget()
		target.Repository = path

		if err := target.Validate(); err != nil {
			t.Errorf("repository %q should be accepted: %v", path, err)
		}
	}
}

// ------------------------------------------------------------ progress --

// Registry-supplied progress text reaches a database column and a browser. It
// is bounded and stripped at this boundary, once, rather than at each of the
// places it could go wrong.
func TestHostileProgressTextIsSanitised(t *testing.T) {
	var seen []PullProgress
	tracker := newProgressTracker(func(progress PullProgress) {
		seen = append(seen, progress)
	})

	tracker.observe("layer1", "Downloading\r\nStatus: pwned", 10, 100)

	if len(seen) != 1 {
		t.Fatalf("expected one emission, got %d", len(seen))
	}
	if strings.ContainsAny(seen[0].Status, "\r\n") {
		t.Errorf("status carries newlines, which would forge a log line: %q", seen[0].Status)
	}
}

func TestOversizedProgressTextIsTruncated(t *testing.T) {
	var seen []PullProgress
	tracker := newProgressTracker(func(progress PullProgress) {
		seen = append(seen, progress)
	})

	tracker.observe("layer1", strings.Repeat("A", 100_000), 0, 0)

	if len(seen) != 1 {
		t.Fatalf("expected one emission, got %d", len(seen))
	}
	if len(seen[0].Status) > maxProgressStatusBytes {
		t.Errorf("status is %d bytes, want at most %d", len(seen[0].Status), maxProgressStatusBytes)
	}
}

// The callback is rate-limited. A pull emits thousands of messages a second,
// and a callback that persisted each one would turn one operator action into an
// unbounded number of writes.
func TestProgressEmissionIsRateLimited(t *testing.T) {
	emitted := 0
	tracker := newProgressTracker(func(PullProgress) { emitted++ })

	for index := 0; index < 5000; index++ {
		tracker.observe("layer1", "Downloading", int64(index), 5000)
	}

	// The first is emitted immediately; the rest fall inside one interval.
	if emitted > 2 {
		t.Errorf("emitted %d progress updates in a tight loop, want at most 2", emitted)
	}
	if emitted == 0 {
		t.Error("no progress was emitted at all")
	}
	if tracker.messages != 5000 {
		t.Errorf("observed %d messages, want every one counted", tracker.messages)
	}
}

// Forwarding stops after a cap, but observation does not: the stream is still
// drained, because abandoning it would leave the daemon writing into a closed
// pipe.
func TestProgressForwardingIsCapped(t *testing.T) {
	emitted := 0
	tracker := newProgressTracker(func(PullProgress) { emitted++ })

	for index := 0; index < maxProgressMessages+500; index++ {
		// The rate limiter is defeated deliberately so the message cap is what
		// is under test.
		tracker.lastEmit = time.Time{}
		tracker.observe("layer", "Downloading", int64(index), 0)
	}

	if emitted > maxProgressMessages {
		t.Errorf("emitted %d updates, want at most the cap of %d", emitted, maxProgressMessages)
	}
	if tracker.messages != maxProgressMessages+500 {
		t.Errorf("stopped counting at %d; the stream must still be drained", tracker.messages)
	}
}

// Layer ids come from the registry, so the set held in memory is bounded. The
// COUNT stays honest past the cap; only the retained ids are limited.
func TestTheTrackedLayerSetIsBounded(t *testing.T) {
	tracker := newProgressTracker(nil)

	const layers = maxTrackedLayers + 250
	for index := 0; index < layers; index++ {
		tracker.observe(strings.Repeat("x", 64)+string(rune(index)), "Downloading", 0, 0)
	}

	if len(tracker.seen) > maxTrackedLayers {
		t.Errorf("retained %d layer ids, want at most %d", len(tracker.seen), maxTrackedLayers)
	}
	if tracker.layers() != layers {
		t.Errorf("layer count = %d, want %d -- the count must stay honest past the cap",
			tracker.layers(), layers)
	}
}

// Bytes are reported as the high-water mark rather than the last value, so a
// message for a small layer does not appear to undo progress.
func TestByteProgressIsAHighWaterMark(t *testing.T) {
	tracker := newProgressTracker(nil)

	tracker.observe("a", "Downloading", 5000, 10000)
	tracker.observe("b", "Downloading", 10, 20)

	if tracker.bytes != 5000 {
		t.Errorf("bytes = %d, want the high-water mark of 5000", tracker.bytes)
	}
}

// A tracker with no callback still counts. The service passes nil when it does
// not want progress, and that must not change what the result reports.
func TestATrackerWithNoCallbackStillCounts(t *testing.T) {
	tracker := newProgressTracker(nil)

	tracker.observe("a", "Downloading", 100, 200)
	tracker.observe("b", "Downloading", 300, 400)

	if tracker.messages != 2 || tracker.layers() != 2 || tracker.bytes != 300 {
		t.Errorf("tracker = %+v", tracker)
	}
}

// -------------------------------------------------------------- fake --

// The fake performs the same validation the real client does, so a test cannot
// accidentally prove that an illegal target is acceptable.
func TestTheFakeAcquirerRefusesAnIllegalTarget(t *testing.T) {
	fake := NewFakeAcquirer()

	target := validTarget()
	target.Digest = ""

	if _, err := fake.PullByDigest(t.Context(), target, nil); err == nil {
		t.Fatal("the fake accepted a target with no digest")
	}
	if fake.CallCount() != 0 {
		t.Error("a refused target must not be recorded as a pull")
	}
}

// ------------------------------------------------------- sanitised text --

// The display sanitiser is what stands between a third party's bytes and a log
// line, a column, and a browser. Its edge cases are worth pinning directly.
func TestDisplayTextSanitisation(t *testing.T) {
	for name, tc := range map[string]struct {
		input string
		limit int
		want  string
	}{
		"plain":                      {"Downloading", 64, "Downloading"},
		"newlines become spaces":     {"one\ntwo", 64, "one two"},
		"control characters dropped": {"a\x00b\x07c", 64, "abc"},
		"C1 controls dropped":        {"\u0085ab", 64, "ab"},
		"trimmed":                    {"  spaced  ", 64, "spaced"},
		"empty":                      {"   ", 64, ""},
		"zero limit":                 {"anything", 0, ""},
		"truncated":                  {"abcdefghij", 4, "abcd"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := domain.SanitiseDisplayText(tc.input, tc.limit); got != tc.want {
				t.Errorf("SanitiseDisplayText(%q, %d) = %q, want %q",
					tc.input, tc.limit, got, tc.want)
			}
		})
	}
}

// Truncation lands on a rune boundary. Cutting mid-sequence would produce
// exactly the invalid UTF-8 this function exists to prevent.
func TestTruncationDoesNotSplitARune(t *testing.T) {
	// Four three-byte runes; a limit of 8 falls inside the third.
	input := "日本語です"

	for limit := 1; limit <= len(input); limit++ {
		got := domain.SanitiseDisplayText(input, limit)
		for _, r := range got {
			if r == '�' {
				t.Fatalf("limit %d produced a replacement character: %q", limit, got)
			}
		}
		if len(got) > limit {
			t.Fatalf("limit %d produced %d bytes", limit, len(got))
		}
	}
}
