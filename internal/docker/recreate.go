package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/moby/moby/api/types/blkiodev"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Container recreation: HarborMaster's second and larger Docker mutation.
//
// # What changed in Phase 9
//
// Phase 8 gave this adapter one mutation -- pulling a digest-pinned image --
// which changes the image store and nothing that is running. Phase 9 adds five,
// and together they can replace a running container.
//
// That is a materially larger privilege, so it is fenced in materially more
// ways:
//
//   - The five methods live on their OWN interface, separate from both the
//     read-only Runtime and the image acquirer. A service that does not receive
//     ContainerMutator cannot touch a container, whatever else it holds.
//   - Every method takes a HARBORMASTER-OWNED request struct. There is no SDK
//     option struct reachable from outside this package, and therefore no field
//     an operator, an API request, or a registry response could fill with an
//     arbitrary Docker parameter.
//   - Every method targets a container by its FULL 64-character id, validated
//     here. Nothing can be aimed by name, so there is no window in which a name
//     resolves to a container other than the one that was checked.
//   - RemoveContainer cannot force, and cannot remove volumes. Both are
//     hardcoded false and there is no field to set them: a container's data is
//     not HarborMaster's to delete, and forcing a removal would skip the "this
//     is stopped" evidence the caller established.
//
// # The configuration never leaves this package
//
// CapturedConfig holds the container's real environment, its log-driver
// options, and the SDK's own config structs in UNEXPORTED fields. The execution
// service receives the value, hands it back to CreateContainer, and cannot read
// it, log it, or serialise it. That is enforced by the compiler, by an
// architecture test that pins the exported surface, and by round-trip tests
// that put a known secret through fmt, slog, and encoding/json.
//
// What the service CAN see is a value-free projection -- environment names,
// mount destinations, network names, capability lists -- which is what the
// preservation check compares and what the UI displays.

// Errors this file can produce.
//
// None of them carries daemon text. An Engine error can embed the socket path,
// the container's command line, and internal state, and these values reach an
// API response and a browser.
var (
	// ErrMutationRefused reports that a request did not describe a legal
	// operation. Raised BEFORE the daemon is contacted: a refusal here means
	// HarborMaster would not have known what it was changing.
	ErrMutationRefused = errors.New("container mutation refused")
	// ErrMutationFailed reports that the daemon could not complete the
	// operation.
	ErrMutationFailed = errors.New("container mutation failed")
	// ErrNameConflict reports that a name the operation needs is already taken.
	ErrNameConflict = errors.New("container name already in use")
	// ErrCaptureFailed reports that a container's configuration could not be
	// read completely enough to reproduce it.
	ErrCaptureFailed = errors.New("container configuration could not be captured")
)

// ------------------------------------------------------- captured config --

// CapturedConfig is one container's configuration, held opaquely.
//
// # Read this before adding a field
//
// Every EXPORTED field here is visible to the execution service, to anything it
// logs, and -- if someone marshals it by accident -- to an API response. The
// unexported ones are not, and that distinction is the whole of the secret
// boundary for this feature.
//
// So the rule is simple and it is tested: an exported field may carry an
// identifier, a digest, or a count. It may never carry an environment value, a
// log-driver option value, a command line, a label value, or anything derived
// from one that could be inverted.
//
// TestCapturedConfigExposesNoSecretSurface in internal/arch pins the exported
// field set and the exported method set. Adding either requires editing a test
// whose entire subject is this limit.
type CapturedConfig struct {
	// ContainerID is the container this was captured from, full length.
	ContainerID string
	// ContainerName is its name, with the Engine's leading slash removed.
	ContainerName string
	// ImageReference is what the container was created from, and ImageID the
	// resolved image. Identifiers, not configuration.
	ImageReference string
	ImageID        string
	// CapturedAt is when the read happened, so a caller can tell how fresh the
	// capture is without trusting its own clock about when it asked.
	CapturedAt time.Time

	// ---- everything below is unreachable outside this package --------------

	// config, host, and networks are the SDK's own structures, already
	// sanitised of runtime-assigned values by captureFrom. They are what
	// CreateContainer sends, and nothing else ever reads them.
	config   *container.Config
	host     *container.HostConfig
	networks *network.NetworkingConfig

	// detail is the normalised view, carrying raw environment values in fields
	// that cannot be serialised (domain.EnvVar.RawValue is `json:"-"`). Used to
	// build the value-free projection.
	//
	// A POINTER, and the indirection is load-bearing rather than incidental.
	// `%#v` is the one rendering Go offers no way to intercept: it reflects
	// over a value and prints unexported fields too. A struct VALUE here would
	// therefore be walked into, and its EnvVar.RawValue printed in full -- so
	// the one format that defeats fmt.Stringer would defeat the whole secret
	// boundary with it. Behind a pointer, `%#v` prints an address.
	//
	// The SDK fields above are already pointers for their own reasons and get
	// the same protection for free. TestCapturedConfigCannotLeakThroughFormatting
	// is what keeps this true.
	detail *domain.ContainerDetail
}

// Valid reports whether the capture is complete enough to create from.
//
// A partial capture is refused rather than completed with defaults. Filling a
// gap with a default would mean creating a container that differs from the one
// it replaces in a way nobody chose.
func (c *CapturedConfig) Valid() bool {
	return c != nil &&
		c.config != nil && c.host != nil &&
		validContainerID(c.ContainerID) &&
		domain.ValidContainerName(c.ContainerName)
}

// Summary projects the configuration into a value-free form.
//
// The ONE way the service learns anything about the contents of a capture.
// Sensitive values contribute a keyed digest produced by the caller's hasher;
// everything else is configuration HarborMaster already displays.
func (c *CapturedConfig) Summary(digest domain.SecretDigester) domain.PreservationSummary {
	if c == nil || c.detail == nil {
		return domain.PreservationSummary{}
	}
	return domain.BuildPreservationSummary(*c.detail, digest)
}

// Detail returns the normalised, MASKED view of the captured container.
//
// Safe to return: domain.ContainerDetail carries sensitive values only in
// EnvVar.RawValue, which is `json:"-"` and therefore cannot be serialised, and
// its Value field already holds the masked form. This is the same type the
// read-only inspection path has always returned.
func (c *CapturedConfig) Detail() domain.ContainerDetail {
	if c == nil || c.detail == nil {
		return domain.ContainerDetail{}
	}
	return *c.detail
}

// LogValue renders the capture for a log record.
//
// Implements slog.LogValuer so that logging a CapturedConfig -- deliberately or
// by including it in a group -- emits identifiers and counts rather than the
// struct. Without this, slog would reflect over the value and reach the
// unexported fields.
func (c *CapturedConfig) LogValue() slog.Value {
	if c == nil {
		return slog.StringValue("<no captured configuration>")
	}
	return slog.GroupValue(
		slog.String("containerId", domain.ShortenID(c.ContainerID)),
		slog.String("containerName", c.ContainerName),
		slog.String("imageId", domain.ShortenID(c.ImageID)),
		slog.Int("environmentCount", len(c.Detail().Environment)),
		slog.Int("mountCount", len(c.Detail().Mounts)),
		slog.Int("networkCount", len(c.Detail().Networks)),
	)
}

// String renders the capture for fmt.
//
// Implements fmt.Stringer so %v and %s produce this rather than a struct dump.
// %#v still reflects -- Go offers no way to intercept it -- which is why the
// unexported fields are the real control and this is a convenience.
func (c *CapturedConfig) String() string {
	if c == nil {
		return "<no captured configuration>"
	}
	return "captured configuration for " + c.ContainerName +
		" (" + domain.ShortenID(c.ContainerID) + ")"
}

// MarshalJSON renders the capture as its identifiers.
//
// Implements json.Marshaler so that a CapturedConfig reaching an encoder --
// through an API response, an error payload, or a log handler that marshals --
// emits identifiers rather than failing or, worse, succeeding with contents.
//
// Note that encoding/json would ALREADY omit the unexported fields. This exists
// so the output is deliberate rather than incidental, and so a future exported
// field does not silently become part of an API response.
func (c *CapturedConfig) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	// Hand-built rather than reflected over a shadow struct, so there is no
	// second type that could drift from this one.
	return []byte(`{"containerId":` + quoteJSON(c.ContainerID) +
		`,"containerName":` + quoteJSON(c.ContainerName) +
		`,"imageReference":` + quoteJSON(c.ImageReference) +
		`,"imageId":` + quoteJSON(c.ImageID) + `}`), nil
}

// quoteJSON renders a string as a JSON string literal.
//
// The values it is given are container identifiers and names, already
// constrained to hex and to domain.ValidContainerName's allowlist. It escapes
// anyway, because a hand-built encoder that assumes its input is safe is how a
// hand-built encoder becomes a vulnerability.
func quoteJSON(value string) string {
	const digits = "0123456789abcdef"

	var builder strings.Builder
	builder.Grow(len(value) + 2)
	builder.WriteByte('"')
	for i := 0; i < len(value); i++ {
		char := value[i]
		switch {
		case char == '"':
			builder.WriteString(`\"`)
		case char == '\\':
			builder.WriteString(`\\`)
		case char < 0x20:
			// The branch bounds char below 0x20, so the escape is always
			// \u00 followed by two digits. Written out rather than formatted:
			// the two nibbles are the whole of it.
			builder.WriteString(`\u00`)
			builder.WriteByte(digits[char>>4])
			builder.WriteByte(digits[char&0xf])
		default:
			builder.WriteByte(char)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// ------------------------------------------------------------ interfaces --

// ConfigCapturer reads a container's configuration for reproduction.
//
// A READ, and deliberately on its own interface rather than on ContainerMutator
// -- a read on the mutation interface would inflate the pinned method count and
// make the architecture test say something less true than it does now.
//
// It is not on Runtime either, because the value it returns is the create
// payload. Only the execution service has any business holding one.
type ConfigCapturer interface {
	// CaptureConfig reads one container's configuration.
	//
	// Changes nothing. The returned value is opaque: see CapturedConfig.
	CaptureConfig(ctx context.Context, containerID string) (*CapturedConfig, error)
}

// ContainerMutator is HarborMaster's ENTIRE ability to change a container.
//
// Five methods. An architecture test asserts that it stays five, asserts their
// exact names, and asserts that no package outside the execution service can
// reach the interface at all.
//
// Note what is absent and cannot be added without amending those tests: no
// exec, no attach, no copy, no commit, no kill, no pause, no update, no
// network connect or disconnect, no volume operation, and nothing at all that
// touches an image.
type ContainerMutator interface {
	// CreateContainer creates a replacement from a captured configuration.
	CreateContainer(ctx context.Context, request CreateRequest) (string, error)
	// StartContainer starts one container by id.
	StartContainer(ctx context.Context, request StartRequest) error
	// StopContainer stops one container by id, within a bounded timeout.
	StopContainer(ctx context.Context, request StopRequest) error
	// RenameContainer renames one container by id.
	RenameContainer(ctx context.Context, request RenameRequest) error
	// RemoveContainer removes one STOPPED container by id, keeping its volumes.
	RemoveContainer(ctx context.Context, request RemoveRequest) error
}

// -------------------------------------------------------------- requests --

// CreateRequest asks for one replacement container.
//
// Three fields, and none of them is a Docker parameter. The configuration is
// the opaque capture, the image is a digest-pinned reference assembled from
// validated components, and the name is validated against the allowlist. There
// is no options field, no command, no mount, no capability, and no privilege
// flag, because there is no field for one.
type CreateRequest struct {
	// Captured is the configuration to reproduce.
	Captured *CapturedConfig
	// Image is the immutable image to create from. Its digest-pinned reference
	// replaces the captured one; everything else is preserved.
	Image domain.ExecutionTarget
	// Name is the name the replacement takes.
	Name string
}

// Validate reports whether the request is safe to send to the daemon.
func (r CreateRequest) Validate() error {
	if !r.Captured.Valid() {
		return fmt.Errorf("%w: the captured configuration is incomplete", ErrMutationRefused)
	}
	if !r.Image.Valid() {
		return fmt.Errorf("%w: the image is not a pinned target", ErrMutationRefused)
	}
	if r.Image.PinnedReference() == "" ||
		len(r.Image.PinnedReference()) > domain.MaxReferenceBytes {
		return fmt.Errorf("%w: the image reference is not acceptable", ErrMutationRefused)
	}
	if !domain.ValidContainerName(r.Name) {
		return fmt.Errorf("%w: the container name is not acceptable", ErrMutationRefused)
	}
	return nil
}

// StartRequest asks for one container to be started.
type StartRequest struct {
	ContainerID string
}

// Validate reports whether the request names one container.
func (r StartRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required", ErrMutationRefused)
	}
	return nil
}

// StopRequest asks for one container to be stopped.
//
// The timeout is how long the container is given to exit on its own before the
// daemon terminates it. Bounded on both sides: an unbounded stop would hold the
// pipeline open indefinitely, and a zero one would kill a container that was
// shutting down cleanly.
type StopRequest struct {
	ContainerID string
	Timeout     time.Duration
}

// Stop timeout bounds.
const (
	// MinStopTimeout is the least grace a container is given.
	MinStopTimeout = 1 * time.Second
	// MaxStopTimeout bounds how long a stop can hold the pipeline.
	MaxStopTimeout = 5 * time.Minute
	// DefaultStopTimeout is used when a caller supplies none.
	DefaultStopTimeout = 30 * time.Second
)

// Validate reports whether the request names one container and a sane grace.
func (r StopRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required", ErrMutationRefused)
	}
	if r.Timeout < 0 || r.Timeout > MaxStopTimeout {
		return fmt.Errorf("%w: the stop timeout is out of range", ErrMutationRefused)
	}
	return nil
}

// RenameRequest asks for one container to be renamed.
type RenameRequest struct {
	ContainerID string
	NewName     string
}

// Validate reports whether the request names one container and a legal name.
func (r RenameRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required", ErrMutationRefused)
	}
	if !domain.ValidContainerName(r.NewName) {
		return fmt.Errorf("%w: the new container name is not acceptable", ErrMutationRefused)
	}
	return nil
}

// RemoveRequest asks for one STOPPED container to be removed.
//
// Note what it does NOT carry, and cannot be made to carry: no force, and no
// remove-volumes. A container's volumes hold its data and are not
// HarborMaster's to delete, and forcing a removal would discard the caller's
// evidence that the container was already stopped.
type RemoveRequest struct {
	ContainerID string
}

// Validate reports whether the request names one container.
func (r RemoveRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required", ErrMutationRefused)
	}
	return nil
}

// validContainerID reports whether id is a full Engine container id.
//
// Exactly 64 lowercase hex characters. Deliberately strict: every id
// HarborMaster passes to a mutation was read from the daemon moments earlier,
// so anything else means a layer above substituted something -- and a mutation
// aimed by a short id or a name is a mutation that could resolve to a different
// container than the one that was checked.
func validContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		char := id[i]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------- the capture --

// CaptureConfig reads one container's configuration for reproduction.
//
// # It changes nothing
//
// One inspect call. The result is normalised through the same path the
// read-only inventory uses, and the SDK structures are copied and stripped of
// values the daemon assigns.
//
// # What is stripped, and why stripping is required rather than optional
//
// An inspection reports both what was CONFIGURED and what the daemon DECIDED.
// Sending the second back would not reproduce the container -- it would pin it
// to another container's decisions:
//
//   - A hostname the daemon derived from the container's own short id would
//     make the replacement believe it is the container it replaced.
//   - IP addresses, gateways, MAC addresses, and endpoint ids belong to a
//     network sandbox that is about to be destroyed. Reusing an address that is
//     still held is a conflict; reusing one that is not is a coincidence.
//
// # Anonymous volumes are made explicit
//
// A volume the daemon created -- from an image VOLUME directive, or from a `-v
// /path` with no source -- is not named in the container's own configuration.
// Recreating naively would give the replacement a BRAND NEW empty volume and
// leave the original's data orphaned, which is data loss dressed up as an
// update.
//
// So each one is converted into an EXPLICIT mount naming the volume that
// already exists. The replacement attaches to the same data, and the
// preservation check -- which compares mount sources -- can then pass honestly
// rather than being taught to ignore the difference.
func (c *Client) CaptureConfig(ctx context.Context, containerID string) (*CapturedConfig, error) {
	if !validContainerID(containerID) {
		return nil, fmt.Errorf("%w: a full container id is required", ErrMutationRefused)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	inspected, err := c.mutateAPI.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrContainerVanished, domain.ShortenID(containerID))
		}
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}

	response := inspected.Container
	if response.Config == nil || response.HostConfig == nil {
		// A capture without both halves cannot reproduce a container. Refused
		// rather than completed with defaults.
		return nil, fmt.Errorf("%w: the daemon did not report a complete configuration", ErrCaptureFailed)
	}

	name := domain.NormaliseContainerName(response.Name)
	if !domain.ValidContainerName(name) {
		return nil, fmt.Errorf("%w: the container's name is not one HarborMaster can reproduce", ErrCaptureFailed)
	}

	normalised := c.normalizeInspection(response, inspected.Raw).Detail

	captured := &CapturedConfig{
		ContainerID:    response.ID,
		ContainerName:  name,
		ImageReference: response.Config.Image,
		ImageID:        response.Image,
		CapturedAt:     time.Now().UTC(),
		detail:         &normalised,
	}

	captured.config = copyConfigForCreate(response)
	captured.host = copyHostConfigForCreate(response)
	captured.networks = copyNetworksForCreate(response)

	return captured, nil
}

// copyConfigForCreate copies the portable configuration, stripped of
// daemon-assigned values.
func copyConfigForCreate(response container.InspectResponse) *container.Config {
	source := response.Config
	// A shallow copy first, then every reference type replaced with its own
	// copy. Sharing a slice or a map with the inspection result would let a
	// later normalisation pass mutate what is about to be sent to the daemon.
	copied := *source

	copied.Env = append([]string(nil), source.Env...)
	copied.Cmd = append([]string(nil), source.Cmd...)
	copied.Entrypoint = append([]string(nil), source.Entrypoint...)
	copied.OnBuild = append([]string(nil), source.OnBuild...)
	copied.Shell = append([]string(nil), source.Shell...)
	copied.Labels = copyStringMap(source.Labels)

	copied.ExposedPorts = make(network.PortSet, len(source.ExposedPorts))
	for port := range source.ExposedPorts {
		copied.ExposedPorts[port] = struct{}{}
	}

	if source.Healthcheck != nil {
		health := *source.Healthcheck
		health.Test = append([]string(nil), source.Healthcheck.Test...)
		copied.Healthcheck = &health
	}
	if source.StopTimeout != nil {
		timeout := *source.StopTimeout
		copied.StopTimeout = &timeout
	}

	// The hostname the daemon derived from the container's own short id is not
	// configuration -- see the note on CaptureConfig. Cleared so the daemon
	// derives a fresh one for the replacement.
	if isGeneratedHostname(copied.Hostname, response.ID) {
		copied.Hostname = ""
	}

	// Anonymous volumes are carried forward as explicit mounts by
	// copyHostConfigForCreate. Leaving them here as well would ask the daemon
	// to create a second, empty volume at the same destination.
	copied.Volumes = anonymousVolumesToKeep(response)

	// Image is set by CreateContainer from the approved digest. Cleared here so
	// a caller that forgot cannot accidentally recreate on the OLD image.
	copied.Image = ""

	return &copied
}

// isGeneratedHostname reports whether a hostname is the daemon's default.
//
// The daemon uses the container's own short id when none is configured.
func isGeneratedHostname(hostname, containerID string) bool {
	if hostname == "" {
		return true
	}
	return len(containerID) >= 12 && hostname == containerID[:12]
}

// anonymousVolumesToKeep returns the Config.Volumes entries that are NOT being
// converted into explicit mounts.
//
// In practice this is empty on most containers. It exists so a destination the
// daemon reported as a volume declaration but did not actually mount -- which
// should not happen, and would be a daemon HarborMaster does not understand --
// is preserved rather than silently dropped.
func anonymousVolumesToKeep(response container.InspectResponse) map[string]struct{} {
	if len(response.Config.Volumes) == 0 {
		return nil
	}

	mounted := make(map[string]struct{}, len(response.Mounts))
	for _, point := range response.Mounts {
		mounted[point.Destination] = struct{}{}
	}

	keep := make(map[string]struct{})
	for destination := range response.Config.Volumes {
		if _, isMounted := mounted[destination]; !isMounted {
			keep[destination] = struct{}{}
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

// copyHostConfigForCreate copies the host configuration and makes anonymous
// volumes explicit.
func copyHostConfigForCreate(response container.InspectResponse) *container.HostConfig {
	source := response.HostConfig
	copied := *source

	copied.Binds = append([]string(nil), source.Binds...)
	copied.CapAdd = append([]string(nil), source.CapAdd...)
	copied.CapDrop = append([]string(nil), source.CapDrop...)
	copied.DNS = append([]netip.Addr(nil), source.DNS...)
	copied.DNSOptions = append([]string(nil), source.DNSOptions...)
	copied.DNSSearch = append([]string(nil), source.DNSSearch...)
	copied.ExtraHosts = append([]string(nil), source.ExtraHosts...)
	copied.GroupAdd = append([]string(nil), source.GroupAdd...)
	copied.Links = append([]string(nil), source.Links...)
	copied.SecurityOpt = append([]string(nil), source.SecurityOpt...)
	copied.VolumesFrom = append([]string(nil), source.VolumesFrom...)
	copied.MaskedPaths = append([]string(nil), source.MaskedPaths...)
	copied.ReadonlyPaths = append([]string(nil), source.ReadonlyPaths...)

	copied.Annotations = copyStringMap(source.Annotations)
	copied.StorageOpt = copyStringMap(source.StorageOpt)
	copied.Sysctls = copyStringMap(source.Sysctls)
	copied.Tmpfs = copyStringMap(source.Tmpfs)

	copied.LogConfig.Config = copyStringMap(source.LogConfig.Config)
	copied.Mounts = append([]mount.Mount(nil), source.Mounts...)

	copied.PortBindings = copyPortMap(source.PortBindings)
	copied.Resources = copyResources(source.Resources)

	if source.Init != nil {
		init := *source.Init
		copied.Init = &init
	}

	copied.Mounts = append(copied.Mounts, implicitVolumeMounts(response)...)

	return &copied
}

// implicitVolumeMounts converts daemon-created volumes into explicit ones.
//
// Each returned mount names the volume that ALREADY EXISTS, so the replacement
// attaches to the same data rather than to a fresh empty volume. See the note
// on CaptureConfig for why this is required rather than a nicety.
//
// Bind mounts are deliberately not touched: a bind's source is a host path and
// is already explicit in Binds or Mounts. tmpfs is not touched either, because
// its contents do not survive the original container by definition.
func implicitVolumeMounts(response container.InspectResponse) []mount.Mount {
	if len(response.Mounts) == 0 {
		return nil
	}

	// Destinations the configuration already covers. A mount that is already
	// explicit must not be added twice: the daemon rejects duplicate targets,
	// and a create that fails after the original is stopped is the worst
	// possible time to discover a bug in this function.
	covered := make(map[string]struct{})
	for _, existing := range response.HostConfig.Mounts {
		covered[existing.Target] = struct{}{}
	}
	for _, bind := range response.HostConfig.Binds {
		if target, ok := bindTarget(bind); ok {
			covered[target] = struct{}{}
		}
	}

	var implicit []mount.Mount
	for _, point := range response.Mounts {
		if point.Type != mount.TypeVolume || point.Name == "" {
			continue
		}
		if _, known := covered[point.Destination]; known {
			continue
		}
		implicit = append(implicit, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   point.Name,
			Target:   point.Destination,
			ReadOnly: !point.RW,
		})
		covered[point.Destination] = struct{}{}
	}
	return implicit
}

// bindTarget extracts the container-side path from a bind specification.
//
// A bind is "source:target" or "source:target:options", and on Windows the
// source itself contains a colon ("C:\data:C:\data"). Parsed from the LEFT
// past any drive letter, which is how the Engine's own parser resolves the
// ambiguity.
func bindTarget(bind string) (string, bool) {
	parts := splitBind(bind)
	if len(parts) < 2 {
		return "", false
	}
	return parts[1], true
}

// splitBind splits a bind specification into at most three parts, treating a
// single-letter prefix followed by a colon as a Windows drive rather than a
// separator.
func splitBind(bind string) []string {
	parts := make([]string, 0, 3)
	current := strings.Builder{}

	for i := 0; i < len(bind); i++ {
		char := bind[i]
		if char != ':' {
			current.WriteByte(char)
			continue
		}
		// A drive letter: exactly one alphabetic character so far, and more to
		// come. "C:\data" is one path, not "C" then "\data".
		if current.Len() == 1 && isDriveLetter(current.String()[0]) && i+1 < len(bind) {
			current.WriteByte(char)
			continue
		}
		if len(parts) == 2 {
			// Everything after the second separator is the option string, which
			// may itself contain colons.
			current.WriteByte(char)
			continue
		}
		parts = append(parts, current.String())
		current.Reset()
	}
	parts = append(parts, current.String())
	return parts
}

func isDriveLetter(char byte) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

// copyNetworksForCreate copies the network attachments, stripped of runtime
// addressing.
//
// Only the CONFIGURED fields survive: aliases, links, driver options, gateway
// priority, and any static IPAM the operator set. Addresses, MAC address,
// endpoint id, network id, and the daemon's generated DNS names are dropped,
// because they describe a sandbox that is about to cease to exist.
func copyNetworksForCreate(response container.InspectResponse) *network.NetworkingConfig {
	settings := response.NetworkSettings
	if settings == nil || len(settings.Networks) == 0 {
		return &network.NetworkingConfig{}
	}

	endpoints := make(map[string]*network.EndpointSettings, len(settings.Networks))
	for name, endpoint := range settings.Networks {
		if endpoint == nil {
			continue
		}
		copied := &network.EndpointSettings{
			Links:      append([]string(nil), endpoint.Links...),
			Aliases:    append([]string(nil), endpoint.Aliases...),
			DriverOpts: copyStringMap(endpoint.DriverOpts),
			GwPriority: endpoint.GwPriority,
		}
		if endpoint.IPAMConfig != nil {
			ipam := *endpoint.IPAMConfig
			ipam.LinkLocalIPs = append([]netip.Addr(nil), endpoint.IPAMConfig.LinkLocalIPs...)
			copied.IPAMConfig = &ipam
		}
		endpoints[name] = copied
	}
	return &network.NetworkingConfig{EndpointsConfig: endpoints}
}

// copyStringMap lives in normalize.go: the same "copy so the capture cannot be
// mutated by a later pass" need, already solved once.

func copyPortMap(source network.PortMap) network.PortMap {
	if source == nil {
		return nil
	}
	copied := make(network.PortMap, len(source))
	for port, bindings := range source {
		copied[port] = append([]network.PortBinding(nil), bindings...)
	}
	return copied
}

func copyResources(source container.Resources) container.Resources {
	copied := source

	copied.BlkioWeightDevice = append([]*blkiodev.WeightDevice(nil), source.BlkioWeightDevice...)
	copied.BlkioDeviceReadBps = append([]*blkiodev.ThrottleDevice(nil), source.BlkioDeviceReadBps...)
	copied.BlkioDeviceWriteBps = append([]*blkiodev.ThrottleDevice(nil), source.BlkioDeviceWriteBps...)
	copied.BlkioDeviceReadIOps = append([]*blkiodev.ThrottleDevice(nil), source.BlkioDeviceReadIOps...)
	copied.BlkioDeviceWriteIOps = append([]*blkiodev.ThrottleDevice(nil), source.BlkioDeviceWriteIOps...)
	copied.Devices = append([]container.DeviceMapping(nil), source.Devices...)
	copied.DeviceCgroupRules = append([]string(nil), source.DeviceCgroupRules...)
	copied.DeviceRequests = append([]container.DeviceRequest(nil), source.DeviceRequests...)
	copied.Ulimits = append([]*container.Ulimit(nil), source.Ulimits...)

	if source.MemorySwappiness != nil {
		swappiness := *source.MemorySwappiness
		copied.MemorySwappiness = &swappiness
	}
	if source.OomKillDisable != nil {
		disable := *source.OomKillDisable
		copied.OomKillDisable = &disable
	}
	if source.PidsLimit != nil {
		limit := *source.PidsLimit
		copied.PidsLimit = &limit
	}
	return copied
}

// ------------------------------------------------------- the five methods --

// CreateContainer creates a replacement from a captured configuration.
//
// # The image is the only thing that changes
//
// Config, HostConfig, and NetworkingConfig are the captured ones. The image is
// replaced with the approved DIGEST-PINNED reference -- not the tag the plan
// displayed -- so the container runs the exact content that was verified
// present on this host, and `docker inspect` afterwards says so.
//
// # It does not start anything
//
// Creating is separate from starting on purpose. A created container holds its
// name and its configuration without running, which gives the pipeline a point
// at which the replacement exists, nothing is serving it, and the decision to
// proceed is still the caller's.
func (c *Client) CreateContainer(ctx context.Context, request CreateRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	captured := request.Captured

	// The config is copied AGAIN here rather than mutated in place, so a
	// CreateRequest can be retried by a caller without the previous attempt's
	// image having been written into the capture.
	config := *captured.config
	config.Image = request.Image.PinnedReference()

	options := client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       captured.host,
		NetworkingConfig: captured.networks,
		Name:             request.Name,
	}
	if platform, ok := ociPlatform(request.Image.Platform); ok {
		options.Platform = &platform
	}

	created, err := c.mutateAPI.ContainerCreate(ctx, options)
	if err != nil {
		return "", classifyMutationError(ctx, err)
	}
	if !validContainerID(created.ID) {
		// The daemon answered with something that is not a container id. Refuse
		// rather than record it: every later mutation targets this value, and
		// one that is not an id would be aimed at nothing or at anything.
		return "", fmt.Errorf("%w: the daemon did not return a usable container id", ErrMutationFailed)
	}
	return created.ID, nil
}

// StartContainer starts one container.
func (c *Client) StartContainer(ctx context.Context, request StartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// ContainerStartOptions carries checkpoint fields, which are left zero.
	// Restoring from a checkpoint is a different capability and HarborMaster
	// has no way to name one.
	if _, err := c.mutateAPI.ContainerStart(ctx, request.ContainerID, client.ContainerStartOptions{}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// StopContainer stops one container, giving it a bounded grace period.
//
// The daemon sends the container's configured stop signal, waits, and
// terminates it if it has not exited. Both halves are bounded: the grace by the
// request, and the whole call by the adapter's own timeout plus a margin, so a
// daemon that never answers cannot hold the pipeline open.
func (c *Client) StopContainer(ctx context.Context, request StopRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	grace := request.Timeout
	if grace <= 0 {
		grace = DefaultStopTimeout
	}
	if grace < MinStopTimeout {
		grace = MinStopTimeout
	}

	// The call has to outlive the grace period it is asking the daemon to
	// observe, or the context would cancel the stop it just requested. The
	// adapter's own timeout is added as the margin for the round trip.
	ctx, cancel := context.WithTimeout(ctx, grace+c.timeout)
	defer cancel()

	seconds := int(grace.Round(time.Second) / time.Second)
	if _, err := c.mutateAPI.ContainerStop(ctx, request.ContainerID, client.ContainerStopOptions{
		Timeout: &seconds,
	}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// RenameContainer renames one container.
func (c *Client) RenameContainer(ctx context.Context, request RenameRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if _, err := c.mutateAPI.ContainerRename(ctx, request.ContainerID, client.ContainerRenameOptions{
		NewName: request.NewName,
	}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// RemoveContainer removes one container, keeping its data.
//
// # Force is off, and there is no way to turn it on
//
// The caller establishes that a container is stopped before removing it. A
// forced removal would skip that evidence and kill something that is running,
// which is the one outcome this whole feature is built to make impossible by
// accident.
//
// # Volumes are kept, and there is no way to remove them
//
// A container's volumes hold its data. HarborMaster replaces containers; it
// does not delete data, and there is no field on RemoveRequest that could ask
// it to.
func (c *Client) RemoveContainer(ctx context.Context, request RemoveRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	_, err := c.mutateAPI.ContainerRemove(ctx, request.ContainerID, client.ContainerRemoveOptions{
		// All three stated explicitly rather than left to the zero value. A
		// reader of this call should not have to know Go's defaults to know
		// that HarborMaster does not force and does not delete data.
		Force:         false,
		RemoveVolumes: false,
		RemoveLinks:   false,
	})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			// Already gone. The caller's intent was that it not be there, and it
			// is not: reporting success here makes the operation idempotent,
			// which is what restart recovery needs.
			return nil
		}
		return classifyMutationError(ctx, err)
	}
	return nil
}

// classifyMutationError maps a daemon failure onto a sanitised error.
//
// The daemon's text is never rendered. An Engine error can embed the socket
// path, the container's command line, and internal daemon state, and the value
// produced here reaches an API response and a browser.
func classifyMutationError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	switch {
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%w: %w", ErrContainerVanished, errors.New("the container is no longer present"))
	case cerrdefs.IsConflict(err):
		// The most useful distinction this function makes. A conflict on create
		// or rename means the name is taken, which is a situation an operator
		// can resolve -- unlike a generic failure.
		return fmt.Errorf("%w: the daemon reported a conflict", ErrNameConflict)
	case cerrdefs.IsInvalidArgument(err):
		return fmt.Errorf("%w: the daemon refused the request", ErrMutationFailed)
	case cerrdefs.IsPermissionDenied(err), cerrdefs.IsUnauthorized(err):
		return fmt.Errorf("%w: the daemon refused the request", ErrMutationFailed)
	case cerrdefs.IsUnavailable(err), cerrdefs.IsDeadlineExceeded(err):
		return fmt.Errorf("%w: %w", ErrUnreachable, errors.New("the daemon did not answer"))
	}
	return fmt.Errorf("%w: the operation did not complete", ErrMutationFailed)
}

// Compile-time checks that Client provides both capabilities.
var (
	_ ConfigCapturer   = (*Client)(nil)
	_ ContainerMutator = (*Client)(nil)
)

// Compile-time check that ocispec stays reachable. ociPlatform lives in
// acquire.go and is shared; this keeps the dependency visible in this file too.
var _ = ocispec.Platform{}
