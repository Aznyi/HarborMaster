package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Container recreation adapter tests.
//
// An INTERNAL test package on purpose. The unexported fields of CapturedConfig
// are the whole secret boundary, and the tests that matter most are the ones
// that put a real secret into those fields and then try every way Go offers to
// get it out again.
//
// What is asserted:
//
//   - A CapturedConfig cannot leak its contents through fmt, slog, or
//     encoding/json.
//   - Every mutation request refuses anything that is not an exact container id
//     and a legal name.
//   - RemoveContainer cannot force and cannot remove volumes, because there is
//     no field for either.
//   - An anonymous volume is carried forward as an explicit mount rather than
//     silently replaced with an empty one.

const (
	testSecret      = "hunter2-the-actual-password"
	testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// capturedWithSecret builds a CapturedConfig holding a known secret in every
// place one can hide.
func capturedWithSecret() *CapturedConfig {
	return &CapturedConfig{
		ContainerID:    testContainerID,
		ContainerName:  "web",
		ImageReference: "nginx:1.27.0",
		ImageID:        "sha256:" + strings.Repeat("a", 64),
		CapturedAt:     time.Unix(0, 0).UTC(),

		config: &container.Config{
			Env:    []string{"DB_PASSWORD=" + testSecret, "PORT=8080"},
			Labels: map[string]string{"app": "web"},
			Cmd:    []string{"nginx", "-g", "daemon off;"},
		},
		host: &container.HostConfig{
			LogConfig: container.LogConfig{
				Type:   "splunk",
				Config: map[string]string{"splunk-token": testSecret},
			},
		},
		networks: &network.NetworkingConfig{},

		detail: &domain.ContainerDetail{
			Overview: domain.ContainerSummary{ID: testContainerID, Name: "web"},
			Environment: []domain.EnvVar{
				{
					Name: "DB_PASSWORD", Value: domain.MaskedValue,
					Sensitivity: domain.SensitivitySensitive, RawValue: testSecret,
				},
				{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
			},
		},
	}
}

// ------------------------------------------------- the secret boundary --

// TestCapturedConfigCannotLeakThroughFormatting is the test this whole design
// exists to make passable.
//
// The execution service holds one of these and hands it back to
// CreateContainer. If any default rendering spilled it, the recreation feature
// would be a credential-disclosure bug wearing a deployment tool's clothes.
func TestCapturedConfigCannotLeakThroughFormatting(t *testing.T) {
	captured := capturedWithSecret()

	// %v and %s go through fmt.Stringer, which is implemented precisely so they
	// do not reflect over the struct.
	for _, format := range []string{"%v", "%s", "%+v"} {
		rendered := fmt.Sprintf(format, captured)
		if strings.Contains(rendered, testSecret) {
			t.Errorf("fmt %s leaked the secret: %s", format, rendered)
		}
	}

	// %#v is the honest exception. Go offers no way to intercept it, so the
	// UNEXPORTED FIELDS are the real control -- and this asserts that the
	// pointer form, which is what a caller holding a *CapturedConfig would
	// print, does not spell out the environment.
	//
	// Recorded rather than skipped: a reader deserves to know exactly where the
	// boundary is, and %#v on a pointer prints an address.
	if rendered := fmt.Sprintf("%#v", captured); strings.Contains(rendered, testSecret) {
		t.Errorf("%%#v on a *CapturedConfig leaked the secret: %s", rendered)
	}
}

// TestCapturedConfigCannotLeakThroughJSON covers the path that reaches an API
// response.
func TestCapturedConfigCannotLeakThroughJSON(t *testing.T) {
	captured := capturedWithSecret()

	encoded, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal captured config: %v", err)
	}
	if bytes.Contains(encoded, []byte(testSecret)) {
		t.Fatalf("encoding/json leaked the secret: %s", encoded)
	}

	// The output must still be usable: an operator debugging a failed
	// recreation needs to know which container it was.
	if !bytes.Contains(encoded, []byte(testContainerID)) {
		t.Error("the marshalled form does not identify the container")
	}

	// Nested inside another value, which is how it would actually reach a
	// response.
	nested, err := json.Marshal(map[string]any{"captured": captured})
	if err != nil {
		t.Fatalf("marshal nested: %v", err)
	}
	if bytes.Contains(nested, []byte(testSecret)) {
		t.Fatalf("a nested CapturedConfig leaked the secret: %s", nested)
	}
}

// TestCapturedConfigCannotLeakThroughSlog covers the logging path.
//
// Without LogValue, slog reflects over the value and would reach the
// unexported fields through its any-handling.
func TestCapturedConfigCannotLeakThroughSlog(t *testing.T) {
	captured := capturedWithSecret()

	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))

	logger.Info("recreating", slog.Any("captured", captured))
	logger.Info("recreating", slog.Any("group", map[string]any{"captured": captured}))

	if strings.Contains(buffer.String(), testSecret) {
		t.Fatalf("slog leaked the secret: %s", buffer.String())
	}
	if !strings.Contains(buffer.String(), "web") {
		t.Error("the log record does not identify the container")
	}
}

// TestCapturedConfigSummaryIsValueFree checks the one channel that is MEANT to
// carry information about the contents.
func TestCapturedConfigSummaryIsValueFree(t *testing.T) {
	captured := capturedWithSecret()

	summary := captured.Summary(func(value string) string { return "digest-of-something" })

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if bytes.Contains(encoded, []byte(testSecret)) {
		t.Fatalf("the value-free projection carried the secret: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte("DB_PASSWORD")) {
		t.Error("the projection dropped the variable name, so a removed secret would be invisible")
	}
}

// TestCapturedConfigDetailIsMasked covers the normalised view the service reads
// for verification.
func TestCapturedConfigDetailIsMasked(t *testing.T) {
	captured := capturedWithSecret()
	detail := captured.Detail()

	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if bytes.Contains(encoded, []byte(testSecret)) {
		t.Fatalf("the normalised detail serialised the raw value: %s", encoded)
	}

	// RawValue is still reachable in memory -- that is how the preservation
	// digest is computed -- and is `json:"-"` so it cannot be serialised. Both
	// halves matter, so both are asserted.
	found := false
	for _, env := range detail.Environment {
		if env.Name == "DB_PASSWORD" {
			found = true
			if env.Value != domain.MaskedValue {
				t.Errorf("the displayed value is %q, want the mask", env.Value)
			}
			if env.RawValue != testSecret {
				t.Error("the raw value is not reachable in memory, so the preservation " +
					"digest could not be computed")
			}
		}
	}
	if !found {
		t.Error("the detail dropped the sensitive variable entirely")
	}
}

func TestNilCapturedConfigRendersSafely(t *testing.T) {
	var captured *CapturedConfig

	if captured.Valid() {
		t.Error("a nil capture reported itself valid")
	}
	if got := captured.String(); got == "" {
		t.Error("a nil capture rendered as the empty string")
	}
	if summary := captured.Summary(nil); len(summary.Fields) != 0 {
		t.Error("a nil capture produced a projection")
	}
	encoded, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal nil capture: %v", err)
	}
	if string(encoded) != "null" {
		t.Errorf("a nil capture marshalled as %s, want null", encoded)
	}
	_ = captured.LogValue()
}

// -------------------------------------------------- request validation --

// TestMutationRequestsRequireAnExactContainerID is what stops a mutation being
// aimed by name.
//
// A short id or a name can resolve to a different container than the one the
// preflight checked. Every mutation therefore targets a full 64-character id,
// which the daemon itself produced moments earlier.
func TestMutationRequestsRequireAnExactContainerID(t *testing.T) {
	bad := []string{
		"", "web", "0123456789ab",
		strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("A", 64), // upper-case hex
		strings.Repeat("g", 64), // not hex
		"../" + strings.Repeat("a", 61),
	}

	for _, id := range bad {
		if err := (StartRequest{ContainerID: id}).Validate(); err == nil {
			t.Errorf("StartRequest accepted container id %q", id)
		}
		if err := (StopRequest{ContainerID: id, Timeout: time.Second}).Validate(); err == nil {
			t.Errorf("StopRequest accepted container id %q", id)
		}
		if err := (RemoveRequest{ContainerID: id}).Validate(); err == nil {
			t.Errorf("RemoveRequest accepted container id %q", id)
		}
		if err := (RenameRequest{ContainerID: id, NewName: "web"}).Validate(); err == nil {
			t.Errorf("RenameRequest accepted container id %q", id)
		}
	}

	if err := (StartRequest{ContainerID: testContainerID}).Validate(); err != nil {
		t.Errorf("a full container id was rejected: %v", err)
	}
}

func TestRenameRequestRejectsAnIllegalName(t *testing.T) {
	for _, name := range []string{
		"", "-flag", ".hidden", "web/../etc", "web name", "web;whoami",
		strings.Repeat("n", domain.MaxContainerNameBytes+1),
	} {
		err := RenameRequest{ContainerID: testContainerID, NewName: name}.Validate()
		if err == nil {
			t.Errorf("RenameRequest accepted the name %q", name)
		}
	}
}

func TestStopRequestBoundsItsTimeout(t *testing.T) {
	if err := (StopRequest{ContainerID: testContainerID, Timeout: -time.Second}).Validate(); err == nil {
		t.Error("StopRequest accepted a negative timeout")
	}
	if err := (StopRequest{ContainerID: testContainerID, Timeout: MaxStopTimeout + time.Second}).Validate(); err == nil {
		t.Error("StopRequest accepted a timeout past the bound; a stop could hold the pipeline open")
	}
	// Zero is legal and means "use the default", which StopContainer applies.
	if err := (StopRequest{ContainerID: testContainerID}).Validate(); err != nil {
		t.Errorf("StopRequest rejected a zero timeout: %v", err)
	}
}

func TestCreateRequestRequiresAPinnedImageAndACompleteCapture(t *testing.T) {
	captured := capturedWithSecret()
	target := domain.ExecutionTarget{
		Registry: "docker.io", Repository: "library/nginx",
		Digest: "sha256:" + strings.Repeat("b", 64),
	}

	valid := CreateRequest{
		Captured: captured, Image: target, Name: "web",
		ExecutionID: "exec_00112233445566778899",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed create request was rejected: %v", err)
	}

	// No capture: nothing to reproduce.
	if err := (CreateRequest{Image: target, Name: "web"}).Validate(); err == nil {
		t.Error("CreateRequest accepted a nil capture")
	}
	// A capture missing its SDK halves cannot create a faithful container.
	partial := &CapturedConfig{ContainerID: testContainerID, ContainerName: "web"}
	if err := (CreateRequest{Captured: partial, Image: target, Name: "web"}).Validate(); err == nil {
		t.Error("CreateRequest accepted an incomplete capture")
	}
	// An unpinned image is the one thing this feature must never create from.
	unpinned := target
	unpinned.Digest = ""
	if err := (CreateRequest{Captured: captured, Image: unpinned, Name: "web"}).Validate(); err == nil {
		t.Error("CreateRequest accepted an image with no digest")
	}
	// An illegal name.
	if err := (CreateRequest{Captured: captured, Image: target, Name: "-flag"}).Validate(); err == nil {
		t.Error("CreateRequest accepted an illegal container name")
	}
}

// TestRemoveRequestCannotForceOrDeleteVolumes is a structural assertion.
//
// It reads as a tautology and is not: it fails the moment somebody adds a
// `Force` or `RemoveVolumes` field, which is exactly the change that needs to
// be visible in review.
func TestRemoveRequestCannotForceOrDeleteVolumes(t *testing.T) {
	requestType := reflect.TypeOf(RemoveRequest{})

	for i := 0; i < requestType.NumField(); i++ {
		name := strings.ToLower(requestType.Field(i).Name)
		switch {
		case strings.Contains(name, "force"):
			t.Error("RemoveRequest gained a force field; HarborMaster only removes containers " +
				"it has already stopped, and forcing would discard that evidence")
		case strings.Contains(name, "volume"):
			t.Error("RemoveRequest gained a volume field; a container's volumes hold its data " +
				"and are not HarborMaster's to delete")
		case strings.Contains(name, "link"):
			t.Error("RemoveRequest gained a link field")
		}
	}
	if requestType.NumField() != 1 {
		t.Errorf("RemoveRequest has %d fields, want exactly 1 (the container id)",
			requestType.NumField())
	}
}

// ------------------------------------------------------ capture fidelity --

// TestGeneratedHostnameIsNotCarriedForward covers the case that would otherwise
// fail every recreation.
//
// The daemon sets Hostname to the container's own short id when none is
// configured. Copying it would give the replacement the OLD container's
// identity, and comparing it would report a difference on every success.
// The second derivation -- the hostname a SHARED NETWORK NAMESPACE assigns --
// has its own file: namespace_hostname_test.go.
func TestGeneratedHostnameIsNotCarriedForward(t *testing.T) {
	// An ordinary container, holding its own network namespace.
	own := &container.HostConfig{NetworkMode: "bridge"}

	if !isGeneratedHostname(testContainerID[:12], testContainerID, own) {
		t.Error("a hostname equal to the container's short id was not recognised as generated")
	}
	if !isGeneratedHostname("", testContainerID, own) {
		t.Error("an empty hostname was not treated as unset")
	}
	if isGeneratedHostname("api.internal", testContainerID, own) {
		t.Error("an operator-chosen hostname was treated as generated and would be dropped")
	}
	// A missing host config establishes nothing about the network mode, so the
	// answer falls back to the id comparison rather than to "derived".
	if isGeneratedHostname("api.internal", testContainerID, nil) {
		t.Error("an operator-chosen hostname was dropped because the host config was absent")
	}
}

// TestAnonymousVolumesAreCarriedForwardExplicitly is the data-loss test.
//
// A volume the daemon created is not named in the container's own
// configuration. Recreating naively gives the replacement a brand new EMPTY
// volume and orphans the original's data, which is data loss dressed up as an
// update.
func TestAnonymousVolumesAreCarriedForwardExplicitly(t *testing.T) {
	response := container.InspectResponse{
		ID: testContainerID,
		Config: &container.Config{
			Volumes: map[string]struct{}{"/data": {}},
		},
		HostConfig: &container.HostConfig{},
		Mounts: []container.MountPoint{
			{
				Type:        mount.TypeVolume,
				Name:        "9f2c1b7ae4d3",
				Destination: "/data",
				RW:          true,
			},
		},
	}

	implicit := implicitVolumeMounts(response)
	if len(implicit) != 1 {
		t.Fatalf("carried forward %d mounts, want 1", len(implicit))
	}
	if implicit[0].Source != "9f2c1b7ae4d3" {
		t.Errorf("the carried-forward mount names volume %q, want the existing one",
			implicit[0].Source)
	}
	if implicit[0].Target != "/data" {
		t.Errorf("the carried-forward mount targets %q, want /data", implicit[0].Target)
	}

	// And the declaration is removed, so the daemon does not ALSO create a
	// fresh anonymous volume at the same destination.
	if keep := anonymousVolumesToKeep(response); len(keep) != 0 {
		t.Errorf("the volume declaration was kept as well as converted: %v", keep)
	}
}

// TestExplicitMountsAreNotDuplicated is the other half: a mount the
// configuration already covers must not be added twice, because the daemon
// rejects duplicate targets and a create that fails after the original is
// stopped is the worst possible moment to find a bug here.
func TestExplicitMountsAreNotDuplicated(t *testing.T) {
	response := container.InspectResponse{
		ID:     testContainerID,
		Config: &container.Config{},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: "data", Target: "/data"},
			},
			Binds: []string{"/host/logs:/logs:ro"},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "data", Destination: "/data", RW: true},
			{Type: mount.TypeBind, Source: "/host/logs", Destination: "/logs"},
		},
	}

	if implicit := implicitVolumeMounts(response); len(implicit) != 0 {
		t.Errorf("duplicated %d already-explicit mounts: %+v", len(implicit), implicit)
	}
}

func TestBindTargetsAreParsedIncludingWindowsDrives(t *testing.T) {
	cases := []struct {
		bind string
		want string
		ok   bool
	}{
		{"/host:/container", "/container", true},
		{"/host:/container:ro", "/container", true},
		{"data:/var/lib/data", "/var/lib/data", true},
		{`C:\host:C:\container`, `C:\container`, true},
		{`C:\host:C:\container:ro`, `C:\container`, true},
		{"/no-target", "", false},
	}

	for _, tc := range cases {
		got, ok := bindTarget(tc.bind)
		if ok != tc.ok {
			t.Errorf("bindTarget(%q) ok = %v, want %v", tc.bind, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("bindTarget(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

// TestRuntimeAddressingIsStrippedFromNetworks stops the replacement being
// pinned to a sandbox that is about to be destroyed.
func TestRuntimeAddressingIsStrippedFromNetworks(t *testing.T) {
	response := container.InspectResponse{
		ID: testContainerID,
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"app": {
					Aliases:    []string{"web"},
					Links:      []string{"db:database"},
					DriverOpts: map[string]string{"opt": "value"},
					NetworkID:  strings.Repeat("d", 64),
					EndpointID: strings.Repeat("e", 64),
					DNSNames:   []string{"web", testContainerID[:12]},
				},
			},
		},
	}

	copied := copyNetworksForCreate(response)
	endpoint, ok := copied.EndpointsConfig["app"]
	if !ok {
		t.Fatal("the network attachment was dropped entirely")
	}

	if endpoint.NetworkID != "" || endpoint.EndpointID != "" {
		t.Error("the sandbox identifiers were carried forward; they belong to a network " +
			"stack that is about to cease to exist")
	}
	if len(endpoint.DNSNames) != 0 {
		t.Error("the daemon-generated DNS names were carried forward")
	}
	if len(endpoint.Aliases) != 1 || endpoint.Aliases[0] != "web" {
		t.Errorf("the configured aliases were not preserved: %v", endpoint.Aliases)
	}
	if len(endpoint.Links) != 1 {
		t.Error("the configured links were not preserved")
	}
	if endpoint.DriverOpts["opt"] != "value" {
		t.Error("the configured driver options were not preserved")
	}
}

// TestTheCapturedImageIsClearedSoItCannotBeReusedByAccident.
//
// Config.Image is set by CreateContainer from the APPROVED digest. Clearing it
// at capture time means a caller that forgot cannot silently recreate on the
// old image.
func TestTheCapturedImageIsClearedSoItCannotBeReusedByAccident(t *testing.T) {
	response := container.InspectResponse{
		ID:         testContainerID,
		Config:     &container.Config{Image: "nginx:1.27.0", Hostname: testContainerID[:12]},
		HostConfig: &container.HostConfig{},
	}

	copied := copyConfigForCreate(response)
	if copied.Image != "" {
		t.Errorf("the captured configuration still names image %q", copied.Image)
	}
	if copied.Hostname != "" {
		t.Error("a daemon-generated hostname was carried forward")
	}
}

// ---------------------------------------------------------- the fake --

// TestFakeMutatorRefusesToRemoveARunningContainer keeps the double honest.
//
// The real adapter cannot force, so the daemon refuses. A fake that removed a
// running container would let a test prove something the production code cannot
// do.
func TestFakeMutatorRefusesToRemoveARunningContainer(t *testing.T) {
	fake := NewFakeMutator()
	id := FakeContainerID(1)
	fake.AddContainer(&FakeContainer{ID: id, Name: "web", Running: true})

	if err := fake.RemoveContainer(context.Background(), RemoveRequest{ContainerID: id}); err == nil {
		t.Fatal("the fake removed a running container; the real adapter cannot")
	}
	if !fake.Present(id) {
		t.Error("the container was removed anyway")
	}
}

// TestFakeMutatorEnforcesNameUniqueness models the daemon's own constraint, so
// a test that forgets to park the original fails here rather than passing
// against something Docker would refuse.
func TestFakeMutatorEnforcesNameUniqueness(t *testing.T) {
	fake := NewFakeMutator()
	original := FakeContainerID(1)
	fake.AddContainer(&FakeContainer{ID: original, Name: "web"})

	_, err := fake.CreateContainer(context.Background(), CreateRequest{
		Captured: capturedWithSecret(),
		Image: domain.ExecutionTarget{
			Registry: "docker.io", Repository: "library/nginx",
			Digest: "sha256:" + strings.Repeat("b", 64),
		},
		Name: "web",
	})
	if err == nil {
		t.Fatal("the fake created a second container under a name already in use")
	}
}

// ------------------------------------------------ the hand-built encoder --

// TestQuoteJSONEscapesEveryControlByte pins the escaping in quoteJSON.
//
// MarshalJSON is hand-built rather than reflected over a shadow struct, so
// there is no second type to drift from -- but a hand-built encoder is only
// safe while its escaping is exhaustive, and nothing was covering the control
// range. The identifiers it quotes are constrained elsewhere; that is a reason
// the escaping is unlikely to be exercised, not a reason for it to be wrong.
//
// Two things are asserted, and the pair is the point:
//
//   - The output decodes back to EXACTLY the bytes that went in. Comparing
//     bytes against encoding/json would be the WRONG test -- the standard
//     library prefers the short forms and both are correct JSON.
//   - The output is byte-identical to the formatted escape. The branch computes
//     two hex nibbles by hand, and an error in either would still produce a
//     plausible escape -- for the wrong character.
func TestQuoteJSONEscapesEveryControlByte(t *testing.T) {
	for b := 0; b < 0x20; b++ {
		input := "a" + string(rune(b)) + "b"

		var decoded string
		if err := json.Unmarshal([]byte(quoteJSON(input)), &decoded); err != nil {
			t.Fatalf("byte %#02x produced JSON that does not parse: %v", b, err)
		}
		if decoded != input {
			t.Errorf("byte %#02x round-tripped to %q, want %q", b, decoded, input)
		}

		// Stated as the escape a formatted implementation produces, so this
		// compares against the obvious version rather than restating the
		// arithmetic and agreeing with itself.
		want := `"` + fmt.Sprintf(`\u%04x`, b) + `"`
		if got := quoteJSON(string(rune(b))); got != want {
			t.Errorf("quoteJSON(%#02x) = %s, want %s", b, got, want)
		}
	}

	for _, tc := range []struct {
		in   string
		want string
	}{
		{`"`, `"\""`},
		{`\`, `"\\"`},
		{"plain", `"plain"`},
		{"", `""`},
	} {
		if got := quoteJSON(tc.in); got != tc.want {
			t.Errorf("quoteJSON(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestCapturedConfigMarshalsToParseableJSON is the same guarantee one level up:
// the four quoted identifiers must compose into a document an ordinary decoder
// accepts, with the captured contents still absent from it.
func TestCapturedConfigMarshalsToParseableJSON(t *testing.T) {
	captured := capturedWithSecret()
	captured.ContainerName = "web\x01odd\"quote"

	encoded, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]string
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("the hand-built document does not parse: %v (%s)", err, encoded)
	}
	if fields["containerName"] != "web\x01odd\"quote" {
		t.Errorf("containerName round-tripped to %q", fields["containerName"])
	}
	if len(fields) != 4 {
		t.Errorf("field count = %d, want 4: %s", len(fields), encoded)
	}
	if strings.Contains(string(encoded), testSecret) {
		t.Error("the marshalled capture carries the secret")
	}
}

// TestAnAutoRemoveContainerCannotBeCaptured: `docker run --rm` sets
// HostConfig.AutoRemove, and the daemon removes such a container the moment it
// exits -- including after the stop the recreation issues. The park rename then
// finds nothing, and the original is gone with only an in-memory capture to
// reconstruct it from. Refused at capture, before anything is stopped.
func TestAnAutoRemoveContainerCannotBeCaptured(t *testing.T) {
	autoRemove := container.InspectResponse{
		ID:         testContainerID,
		Name:       "/web",
		HostConfig: &container.HostConfig{AutoRemove: true},
		Config:     &container.Config{},
	}
	err := captureRefusal(autoRemove)
	if !errors.Is(err, ErrCaptureFailed) {
		t.Fatalf("an AutoRemove container was accepted for capture: %v", err)
	}

	ordinary := autoRemove
	ordinary.HostConfig = &container.HostConfig{}
	if err := captureRefusal(ordinary); err != nil {
		t.Fatalf("an ordinary container was refused: %v", err)
	}
}

// TestMutationCallsAreNotBoundedByTheInspectionTimeout: the adapter's request
// timeout is sized for an inspect. A create or a start on a loaded host can
// legitimately take longer, and a client-side deadline there is a failure
// AFTER the mutation point while the daemon usually completes the request
// anyway -- the one arrangement no recovery path can repair.
func TestMutationCallsAreNotBoundedByTheInspectionTimeout(t *testing.T) {
	short := &Client{timeout: time.Second}
	if got := short.mutationTimeout(); got != MinMutationCallTimeout {
		t.Errorf("mutation timeout %v with a %v request timeout, want the floor %v",
			got, short.timeout, MinMutationCallTimeout)
	}
	long := &Client{timeout: 10 * time.Minute}
	if got := long.mutationTimeout(); got != long.timeout {
		t.Errorf("mutation timeout %v, want the configured %v", got, long.timeout)
	}
}

// TestACreateWritesOwnershipLabelsOverAnyClaimant: the two labels that identify
// a replacement as this execution's are HarborMaster's own. A source container
// carrying either -- copied from an earlier recreation, or planted -- does not
// get to decide what the replacement claims about itself.
func TestACreateWritesOwnershipLabelsOverAnyClaimant(t *testing.T) {
	const executionID = "exec_00112233445566778899"
	source := map[string]string{
		"app":                      "web",
		domain.LabelExecutionOwner: "exec_ffffffffffffffffffff",
		domain.LabelReplacementOf:  strings.Repeat("f", 64),
		domain.LineageLabel:        "docker.io/library/nginx:1.27",
	}

	labels := ownershipLabels(source, executionID, testContainerID)

	if labels[domain.LabelExecutionOwner] != executionID {
		t.Errorf("execution label = %q, want %q", labels[domain.LabelExecutionOwner], executionID)
	}
	if labels[domain.LabelReplacementOf] != testContainerID {
		t.Errorf("original label = %q, want the captured container", labels[domain.LabelReplacementOf])
	}
	if labels["app"] != "web" || labels[domain.LineageLabel] != "docker.io/library/nginx:1.27" {
		t.Error("an unrelated label was lost")
	}
	// The source map is untouched: a retry must create from the capture as it
	// was, not from one the previous attempt edited.
	if source[domain.LabelExecutionOwner] != "exec_ffffffffffffffffffff" {
		t.Error("the captured labels were mutated in place")
	}
}

// TestCreateRequestRequiresTheExecutionThatOwnsIt: a replacement created
// without ownership is one that can never be adopted after a crash, which is
// the gap the labels close. The adapter refuses the request rather than
// creating an orphan.
func TestCreateRequestRequiresTheExecutionThatOwnsIt(t *testing.T) {
	captured := &CapturedConfig{
		ContainerID: testContainerID, ContainerName: "web",
		config: &container.Config{}, host: &container.HostConfig{},
	}
	target := domain.ExecutionTarget{
		Registry: "docker.io", Repository: "library/nginx",
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
	valid := CreateRequest{Captured: captured, Image: target, Name: "web", ExecutionID: "exec_00112233445566778899"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a request naming its execution was refused: %v", err)
	}
	for name, request := range map[string]CreateRequest{
		"no execution":        {Captured: captured, Image: target, Name: "web"},
		"malformed execution": {Captured: captured, Image: target, Name: "web", ExecutionID: "exec_zz"},
	} {
		if err := request.Validate(); !errors.Is(err, ErrMutationRefused) {
			t.Errorf("%s: got %v, want a refusal", name, err)
		}
	}
}

// TestEveryNetworkAndItsStaticAddressingSurviveTheCopy: a container on more
// than one user-defined network, one of them with a static IPAM address,
// must be created onto all of them with the same static address. Only the
// sandbox-assigned values are dropped.
func TestEveryNetworkAndItsStaticAddressingSurviveTheCopy(t *testing.T) {
	static := netip.MustParseAddr("10.20.0.42")
	static6 := netip.MustParseAddr("fd00::42")
	linkLocal := netip.MustParseAddr("169.254.1.1")

	response := container.InspectResponse{
		ID: testContainerID,
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"frontend": {
					Aliases:    []string{"web", "www"},
					GwPriority: 10,
					IPAddress:  netip.MustParseAddr("172.20.0.5"),
					Gateway:    netip.MustParseAddr("172.20.0.1"),
				},
				"backend": {
					Aliases: []string{"web-internal"},
					IPAMConfig: &network.EndpointIPAMConfig{
						IPv4Address:  static,
						IPv6Address:  static6,
						LinkLocalIPs: []netip.Addr{linkLocal},
					},
					IPAddress: static,
				},
			},
		},
	}

	copied := copyNetworksForCreate(response)
	if len(copied.EndpointsConfig) != 2 {
		t.Fatalf("%d endpoints were copied, want 2", len(copied.EndpointsConfig))
	}

	frontend := copied.EndpointsConfig["frontend"]
	if frontend == nil || len(frontend.Aliases) != 2 || frontend.GwPriority != 10 {
		t.Errorf("the frontend attachment lost configuration: %+v", frontend)
	}
	if frontend != nil && (frontend.IPAddress.IsValid() || frontend.Gateway.IsValid()) {
		t.Error("a sandbox-assigned address on the frontend was carried forward")
	}

	backend := copied.EndpointsConfig["backend"]
	if backend == nil || backend.IPAMConfig == nil {
		t.Fatalf("the backend's static addressing was dropped: %+v", backend)
	}
	if backend.IPAMConfig.IPv4Address != static || backend.IPAMConfig.IPv6Address != static6 {
		t.Errorf("static addresses %v/%v, want %v/%v",
			backend.IPAMConfig.IPv4Address, backend.IPAMConfig.IPv6Address, static, static6)
	}
	if len(backend.IPAMConfig.LinkLocalIPs) != 1 || backend.IPAMConfig.LinkLocalIPs[0] != linkLocal {
		t.Errorf("link-local addresses %v, want [%v]", backend.IPAMConfig.LinkLocalIPs, linkLocal)
	}
	// The copy is a copy: editing it must not reach the inspection.
	backend.IPAMConfig.IPv4Address = netip.MustParseAddr("10.20.0.99")
	if response.NetworkSettings.Networks["backend"].IPAMConfig.IPv4Address != static {
		t.Error("the copied IPAM configuration aliases the inspection's")
	}
}

// TestPortsAndTheRestartPolicySurviveTheCopy: exposed ports, published port
// bindings, and the restart policy are all reproduced on the create request
// exactly, and as copies.
func TestPortsAndTheRestartPolicySurviveTheCopy(t *testing.T) {
	http := network.MustParsePort("80/tcp")
	metrics := network.MustParsePort("9090/tcp")
	dns := network.MustParsePort("53/udp")

	response := container.InspectResponse{
		ID: testContainerID,
		Config: &container.Config{
			ExposedPorts: network.PortSet{http: {}, metrics: {}, dns: {}},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				http: {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
				dns:  {{HostPort: "5353"}, {HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "5354"}},
			},
			PublishAllPorts: true,
			RestartPolicy:   container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 4},
		},
	}

	config := copyConfigForCreate(response)
	if len(config.ExposedPorts) != 3 {
		t.Errorf("%d exposed ports, want 3", len(config.ExposedPorts))
	}
	for _, port := range []network.Port{http, metrics, dns} {
		if _, ok := config.ExposedPorts[port]; !ok {
			t.Errorf("exposed port %s was dropped", port)
		}
	}

	host := copyHostConfigForCreate(response)
	if len(host.PortBindings[http]) != 1 || host.PortBindings[http][0].HostPort != "8080" {
		t.Errorf("the http binding was not preserved: %+v", host.PortBindings[http])
	}
	if len(host.PortBindings[dns]) != 2 {
		t.Errorf("the dns bindings were not preserved: %+v", host.PortBindings[dns])
	}
	if !host.PublishAllPorts {
		t.Error("publish-all was dropped")
	}
	if host.RestartPolicy.Name != container.RestartPolicyOnFailure || host.RestartPolicy.MaximumRetryCount != 4 {
		t.Errorf("restart policy %+v, want on-failure:4", host.RestartPolicy)
	}

	// Copies, not aliases.
	host.PortBindings[http][0].HostPort = "9999"
	if response.HostConfig.PortBindings[http][0].HostPort != "8080" {
		t.Error("the copied port bindings alias the inspection's")
	}
	delete(config.ExposedPorts, http)
	if _, still := response.Config.ExposedPorts[http]; !still {
		t.Error("the copied exposed ports alias the inspection's")
	}
}
