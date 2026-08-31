package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrAmbiguousID reports that a short container ID prefix matched more than
// one container.
var ErrAmbiguousID = errors.New("ambiguous container id")

// SortDirection is an ordering direction.
type SortDirection string

// Sort directions.
const (
	SortAsc  SortDirection = "asc"
	SortDesc SortDirection = "desc"
)

// sortColumns is the allowlist mapping API sort fields to SQL columns.
//
// This map is the ONLY way a sort field reaches the query. Nothing from the
// request is ever interpolated into SQL: an unknown field is rejected by the
// handler, and a known one is replaced by the trusted string here.
var sortColumns = map[string]string{
	"name":         "c.name",
	"state":        "c.state",
	"health":       "c.health",
	"created":      "c.created_at",
	"started":      "c.started_at",
	"image":        "c.image_repository",
	"project":      "c.compose_project",
	"service":      "c.compose_service",
	"restartCount": "c.restart_count",
	"lastSeen":     "c.last_seen_at",
}

// SortFields returns the sortable field names, for validation and docs.
func SortFields() []string {
	fields := make([]string, 0, len(sortColumns))
	for field := range sortColumns {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// ValidSortField reports whether a field name is sortable.
func ValidSortField(field string) bool {
	_, ok := sortColumns[field]
	return ok
}

// ContainerFilter selects and orders containers for the list endpoint.
type ContainerFilter struct {
	// Search matches name, image reference, or ID prefix.
	Search string
	// States and Health are OR-ed within themselves, AND-ed with everything else.
	States []domain.ContainerState
	Health []domain.HealthState

	ComposeProject string
	ComposeService string
	// Image matches the image repository or the full reference.
	Image         string
	RestartPolicy string
	// HarborMasterEnabled filters on the io.harbormaster.enabled label. Nil
	// means "do not filter"; false means "explicitly disabled", which is not
	// the same as the label being absent.
	HarborMasterEnabled *bool

	LabelKey   string
	LabelValue string

	// IncludeAbsent includes containers the most recent refresh did not see.
	IncludeAbsent bool

	// ExcludePreserved drops the containers HarborMaster itself parked aside
	// as evidence -- an original held during an update, a replacement that
	// failed verification, a replacement a rollback moved aside.
	//
	// The default view of a host is its WORKLOADS. Those three are none of
	// them: they are stopped on purpose, they run a deliberately older image,
	// and listing them beside real services was the audit finding this
	// answers. They remain one checkbox away, and nothing deletes them.
	//
	// Exclusion is by RECORD, never by the shape of a name.
	ExcludePreserved bool

	Sort      string
	Direction SortDirection
	Page      Page
}

// ContainerRepository reads the persisted container inventory.
type ContainerRepository struct {
	db *sql.DB
}

const containerColumns = `
	c.id, c.host_id, c.short_id, c.name, c.image_id, c.image_ref,
	c.image_repository, c.image_tag, c.image_digest,
	c.state, c.status, c.health, c.created_at, c.started_at, c.finished_at,
	c.exit_code, c.restart_count, c.restart_policy_name, c.restart_policy_max_retry,
	c.compose_project, c.compose_service, c.compose_container_number, c.compose_oneoff,
	c.hm_enabled, c.ports, c.present, c.first_seen_at, c.last_seen_at,
	c.generation, c.warning_count`

// List returns a page of containers and the total matching the filter.
//
// Two queries, always: one for the page and one for the count. Ports travel as
// JSON on the row and labels are not needed for a list, so there is no per-row
// follow-up query -- the cost does not grow with page size.
func (r *ContainerRepository) List(ctx context.Context, filter ContainerFilter) ([]domain.ContainerSummary, int, error) {
	where, args := buildContainerWhere(filter)
	page := filter.Page.normalise()

	var total int
	countQuery := `SELECT COUNT(*) FROM containers c` + labelJoin(filter) + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count containers: %w", err)
	}

	query := `SELECT` + containerColumns + ` FROM containers c` + labelJoin(filter) + where +
		buildContainerOrder(filter) + ` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query containers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	summaries := make([]domain.ContainerSummary, 0)
	for rows.Next() {
		summary, err := scanContainerSummary(rows)
		if err != nil {
			return nil, 0, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, total, rows.Err()
}

// labelJoin adds the label table only when a label filter is in play, so the
// common case stays a single-table scan.
func labelJoin(filter ContainerFilter) string {
	if filter.LabelKey == "" {
		return ""
	}
	return ` JOIN container_labels l ON l.container_id = c.id`
}

// buildContainerWhere assembles the predicate. Every value is a bound
// parameter; nothing from the caller is concatenated into SQL.
func buildContainerWhere(filter ContainerFilter) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)

	if !filter.IncludeAbsent {
		clauses = append(clauses, "c.present = 1")
	}

	if filter.ExcludePreserved {
		clauses = append(clauses, "c.name NOT IN ("+preservedNameSet+")")
	}

	if search := strings.TrimSpace(filter.Search); search != "" {
		clauses = append(clauses,
			"(c.name LIKE ? ESCAPE '\\' OR c.image_ref LIKE ? ESCAPE '\\' OR c.id LIKE ? ESCAPE '\\')")
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern, escapeLike(search)+"%")
	}

	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			placeholders[i] = "?"
			args = append(args, string(state))
		}
		clauses = append(clauses, "c.state IN ("+strings.Join(placeholders, ",")+")")
	}

	if len(filter.Health) > 0 {
		placeholders := make([]string, len(filter.Health))
		for i, health := range filter.Health {
			placeholders[i] = "?"
			args = append(args, string(health))
		}
		clauses = append(clauses, "c.health IN ("+strings.Join(placeholders, ",")+")")
	}

	if filter.ComposeProject != "" {
		clauses = append(clauses, "c.compose_project = ?")
		args = append(args, filter.ComposeProject)
	}
	if filter.ComposeService != "" {
		clauses = append(clauses, "c.compose_service = ?")
		args = append(args, filter.ComposeService)
	}
	if filter.Image != "" {
		clauses = append(clauses, "(c.image_repository = ? OR c.image_ref = ?)")
		args = append(args, filter.Image, filter.Image)
	}
	if filter.RestartPolicy != "" {
		clauses = append(clauses, "c.restart_policy_name = ?")
		args = append(args, filter.RestartPolicy)
	}
	if filter.HarborMasterEnabled != nil {
		clauses = append(clauses, "c.hm_enabled = ?")
		args = append(args, *filter.HarborMasterEnabled)
	}

	if filter.LabelKey != "" {
		clauses = append(clauses, "l.key = ?")
		args = append(args, filter.LabelKey)
		if filter.LabelValue != "" {
			clauses = append(clauses, "l.value = ?")
			args = append(args, filter.LabelValue)
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// escapeLike neutralises LIKE wildcards in user input, so a search for "%"
// matches a literal percent sign instead of everything.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// buildContainerOrder resolves the sort field through the allowlist and always
// appends a deterministic tiebreak, so paging cannot repeat or skip rows.
func buildContainerOrder(filter ContainerFilter) string {
	column, ok := sortColumns[filter.Sort]
	if !ok {
		column = sortColumns["name"]
	}

	direction := "ASC"
	if filter.Direction == SortDesc {
		direction = "DESC"
	}

	if column == "c.name" {
		return " ORDER BY c.name " + direction + ", c.id ASC"
	}
	return " ORDER BY " + column + " " + direction + ", c.name ASC, c.id ASC"
}

// Get returns HarborMaster's RECORD for one container id, present or not.
//
// # This is not a presence check
//
// The inventory deliberately keeps a container's row after it leaves the host,
// with present = 0, until retention purges it. That row is what a detail page,
// an Activity entry, an execution record and an audit trail read afterwards, so
// this returns it. A successful Get therefore means "HarborMaster has a record
// of this id" and NOTHING about whether the container exists now.
//
// Treating the two as the same is not hypothetical: planEvidence.ContainerPresent
// did exactly that, and a departed container passed an acquisition presence gate
// on the strength of its own tombstone. Callers that need current presence use
// GetPresent below, so the intent is visible in the call rather than in a field
// check somebody has to remember.
//
// Callers that legitimately want the historical row -- detail, history,
// diagnostics, audit, old-plan inspection -- keep using this.
func (r *ContainerRepository) Get(ctx context.Context, id string) (*domain.ContainerDetail, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT`+containerColumns+` FROM containers c WHERE c.id = ?`, id)

	summary, err := scanContainerSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	detail := &domain.ContainerDetail{
		Overview:     summary,
		Compose:      summary.Compose,
		HarborMaster: summary.HarborMaster,
		Ports:        summary.Ports,
		Environment:  []domain.EnvVar{},
		Labels:       []domain.Label{},
		Mounts:       []domain.Mount{},
		Networks:     []domain.NetworkAttachment{},
		Warnings:     []domain.InventoryWarning{},
	}

	if err := r.loadConfig(ctx, detail, id); err != nil {
		return nil, err
	}
	if err := r.loadLabels(ctx, detail, id); err != nil {
		return nil, err
	}
	if err := r.loadNetworks(ctx, detail, id); err != nil {
		return nil, err
	}
	if err := r.loadMounts(ctx, detail, id); err != nil {
		return nil, err
	}
	if err := r.loadRunningDigest(ctx, detail, id); err != nil {
		return nil, err
	}
	return detail, nil
}

// loadRunningDigest resolves what this container is ACTUALLY running.
//
// Separate from the summary row because the answer does not live on the
// container: for a tag-created container it lives on the IMAGE, in the
// RepoDigests the daemon reported, and only domain.RunningDigestFor may decide
// which of them applies to this repository.
//
// A container whose image the inventory no longer holds resolves to empty
// rather than failing. An unestablished digest is a handled outcome everywhere
// it is read; a failed container lookup would not be.
func (r *ContainerRepository) loadRunningDigest(
	ctx context.Context, detail *domain.ContainerDetail, id string,
) error {
	var repoDigests string
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(im.repo_digests, '[]')
		FROM containers c
		LEFT JOIN images im ON im.id = c.image_id
		WHERE c.id = ?`, id).Scan(&repoDigests)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read running digest: %w", AsError(err))
	}

	var digests []string
	if repoDigests != "" {
		// A malformed column is an absent list, not a failure.
		_ = json.Unmarshal([]byte(repoDigests), &digests)
	}

	declared, parseErr := domain.NormalizeImageRef(detail.Overview.Image.Raw)
	if parseErr != nil {
		// A reference this build will not parse cannot be matched against a
		// RepoDigest safely, so nothing is claimed and the digest stays
		// unestablished. Deliberately not an error: one container with an odd
		// reference must not make the whole detail lookup fail, and every
		// consumer already treats an empty running digest as "cannot assess".
		//nolint:nilerr // an unparseable reference is an unknown digest, not a failure.
		return nil
	}
	digest, _ := domain.RunningDigestFor(declared, digests)
	detail.RunningDigest = digest
	return nil
}

// GetPresent returns the detail for one container ONLY while it is on the host.
//
// # The contract
//
//	no row            -> ErrNotFound
//	row, present = 0  -> ErrNotFound
//	row, present = 1  -> the detail
//
// Both absences collapse to ErrNotFound deliberately. A caller asking this
// question wants to know whether it may act, and "there is no such container"
// and "there was one and it is gone" are the same answer to that: no. Callers
// that need to tell them apart are asking a historical question and use Get.
//
// # Why this exists as an operation rather than a field check
//
// It was a field check, and it was forgotten. The presence gate in front of
// image acquisition read only whether Get returned a row, so a container that
// had been removed from the host still passed it. The check was one line away
// and the name promised it had been done.
//
// An operation makes the intent structural: a reader sees which question the
// call site is asking without inspecting what it does with the result, and a
// new mutation path gets the safe answer by picking the obviously named method.
//
// Filtered in SQL, so this costs exactly what Get costs.
func (r *ContainerRepository) GetPresent(
	ctx context.Context, id string,
) (*domain.ContainerDetail, error) {
	var present int
	err := r.db.QueryRowContext(ctx,
		`SELECT present FROM containers WHERE id = ?`, id).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read container presence: %w", AsError(err))
	}
	if present != 1 {
		return nil, ErrNotFound
	}
	return r.Get(ctx, id)
}

func (r *ContainerRepository) loadConfig(ctx context.Context, detail *domain.ContainerDetail, id string) error {
	var encoded string
	err := r.db.QueryRowContext(ctx,
		`SELECT config_json FROM container_config WHERE container_id = ?`, id).Scan(&encoded)

	// A container recorded from summary data alone has no config row. That is
	// a degraded record, not an error.
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read container config: %w", err)
	}

	var config persistedConfig
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		// A corrupt config row degrades this one container rather than failing
		// the request: the identity, state, and labels above are still worth
		// showing, and a detail page that 500s tells an operator nothing.
		//
		// It is NOT swallowed silently, though. Swallowing it would render a
		// container with a blank configuration and no explanation, which reads
		// as "this container has no configuration" -- a materially wrong answer.
		// The warning is what makes the difference visible in the UI.
		detail.Warnings = append(detail.Warnings, domain.InventoryWarning{
			ContainerID:   id,
			ContainerName: detail.Overview.Name,
			Code:          domain.WarningIncompleteData,
			Message:       "stored configuration could not be decoded; showing summary data only",
			OccurredAt:    time.Now().UTC(),
		})
		return nil //nolint:nilerr // deliberate degradation, recorded as a warning above
	}

	detail.State = config.State
	detail.Process = config.Process
	detail.HealthCheck = config.HealthCheck
	detail.Resources = config.Resources
	detail.Security = config.Security
	detail.Logging = config.Logging
	if config.Environment != nil {
		detail.Environment = config.Environment
	}
	if config.Compose.Project != "" {
		detail.Compose = config.Compose
	}
	return nil
}

func (r *ContainerRepository) loadLabels(ctx context.Context, detail *domain.ContainerDetail, id string) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value, source FROM container_labels WHERE container_id = ? ORDER BY key`, id)
	if err != nil {
		return fmt.Errorf("read container labels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			label  domain.Label
			source string
		)
		if err := rows.Scan(&label.Key, &label.Value, &source); err != nil {
			return err
		}
		label.Source = domain.LabelSource(source)
		detail.Labels = append(detail.Labels, label)
	}
	return rows.Err()
}

func (r *ContainerRepository) loadNetworks(ctx context.Context, detail *domain.ContainerDetail, id string) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT network_name, network_id, aliases, ipv4_address, ipv6_address,
		       gateway, mac_address, endpoint_id, links
		FROM container_networks WHERE container_id = ? ORDER BY network_name`, id)
	if err != nil {
		return fmt.Errorf("read container networks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			attachment  domain.NetworkAttachment
			aliasesJSON string
			linksJSON   string
		)
		if err := rows.Scan(&attachment.NetworkName, &attachment.NetworkID, &aliasesJSON,
			&attachment.IPv4Address, &attachment.IPv6Address, &attachment.Gateway,
			&attachment.MACAddress, &attachment.EndpointID, &linksJSON); err != nil {
			return err
		}
		attachment.Aliases = unmarshalStrings(aliasesJSON)
		attachment.Links = unmarshalStrings(linksJSON)
		detail.Networks = append(detail.Networks, attachment)
	}
	return rows.Err()
}

func (r *ContainerRepository) loadMounts(ctx context.Context, detail *domain.ContainerDetail, id string) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT destination, type, source, read_only, propagation, consistency,
		       volume_name, driver, driver_options, tmpfs_options
		FROM container_mounts WHERE container_id = ? ORDER BY destination`, id)
	if err != nil {
		return fmt.Errorf("read container mounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			mount       domain.Mount
			mountType   string
			optionsJSON string
		)
		if err := rows.Scan(&mount.Destination, &mountType, &mount.Source, &mount.ReadOnly,
			&mount.Propagation, &mount.Consistency, &mount.VolumeName, &mount.Driver,
			&optionsJSON, &mount.TmpfsOptions); err != nil {
			return err
		}
		mount.Type = domain.MountType(mountType)
		mount.DriverOptions = unmarshalStringMap(optionsJSON)
		detail.Mounts = append(detail.Mounts, mount)
	}
	return rows.Err()
}

// RawInspection returns the redacted raw payload for a container.
//
// Served only from its own endpoint, never as part of the container detail
// response, so a client that just wants the normalized view never receives it.
func (r *ContainerRepository) RawInspection(ctx context.Context, id string) ([]byte, error) {
	var raw sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT raw_json FROM container_config WHERE container_id = ?`, id).Scan(&raw)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read raw inspection: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return nil, ErrNotFound
	}
	return []byte(raw.String), nil
}

// ResolveID maps a full or short container ID onto the full ID.
//
// A prefix matching several containers returns ErrAmbiguousID rather than an
// arbitrary one of them: silently picking a container is exactly the kind of
// guess an operations tool must not make.
func (r *ContainerRepository) ResolveID(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", ErrNotFound
	}

	// Exact match wins outright, so a full ID never becomes ambiguous just
	// because it prefixes another.
	var exact string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM containers WHERE id = ?`, reference).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve container id: %w", err)
	}

	// Then an exact name match, which is how operators usually refer to a
	// container.
	err = r.db.QueryRowContext(ctx, `SELECT id FROM containers WHERE name = ?`, reference).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve container name: %w", err)
	}

	// Finally a prefix. Two rows are fetched so ambiguity is detectable.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM containers WHERE id LIKE ? ESCAPE '\' ORDER BY id LIMIT 2`,
		escapeLike(reference)+"%")
	if err != nil {
		return "", fmt.Errorf("resolve container prefix: %w", err)
	}
	defer func() { _ = rows.Close() }()

	matches := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return "", ErrAmbiguousID
	}
}

// DistinctComposeProjects lists the Compose projects present, for the UI's
// project filter.
func (r *ContainerRepository) DistinctComposeProjects(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT compose_project FROM containers
		WHERE present = 1 AND compose_project <> ''
		ORDER BY compose_project`)
	if err != nil {
		return nil, fmt.Errorf("query compose projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	projects := make([]string, 0)
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// DistinctImages lists the image repositories present, for the UI's image filter.
func (r *ContainerRepository) DistinctImages(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT image_repository FROM containers
		WHERE present = 1 AND image_repository <> ''
		ORDER BY image_repository`)
	if err != nil {
		return nil, fmt.Errorf("query images: %w", err)
	}
	defer func() { _ = rows.Close() }()

	images := make([]string, 0)
	for rows.Next() {
		var image string
		if err := rows.Scan(&image); err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func scanContainerSummary(row rowScanner) (domain.ContainerSummary, error) {
	var (
		summary     domain.ContainerSummary
		state       string
		health      string
		createdAt   string
		startedAt   sql.NullString
		finishedAt  sql.NullString
		exitCode    sql.NullInt64
		hmEnabled   sql.NullBool
		portsJSON   string
		firstSeenAt string
		lastSeenAt  string
		oneOff      bool
	)

	if err := row.Scan(
		&summary.ID, &summary.HostID, &summary.ShortID, &summary.Name,
		&summary.ImageID, &summary.Image.Raw, &summary.Image.Repository,
		&summary.Image.Tag, &summary.Image.Digest,
		&state, &summary.Status, &health, &createdAt, &startedAt, &finishedAt,
		&exitCode, &summary.RestartCount, &summary.RestartPolicy.Name,
		&summary.RestartPolicy.MaximumRetryCount,
		&summary.Compose.Project, &summary.Compose.Service,
		&summary.Compose.ContainerNumber, &oneOff,
		&hmEnabled, &portsJSON, &summary.Present, &firstSeenAt, &lastSeenAt,
		&summary.Generation, &summary.WarningCount,
	); err != nil {
		return domain.ContainerSummary{}, err
	}

	summary.State = domain.ContainerState(state)
	summary.Health = domain.HealthState(health)
	summary.Compose.OneOff = oneOff
	summary.Compose.Managed = summary.Compose.Project != ""
	summary.HarborMaster.Enabled = scanOptionalBool(hmEnabled)
	summary.ExitCode = scanOptionalInt(exitCode)
	summary.StartedAt = scanOptionalTime(startedAt)
	summary.FinishedAt = scanOptionalTime(finishedAt)

	if parsed, err := parseTime(createdAt); err == nil {
		summary.CreatedAt = parsed
	}
	summary.FirstSeenAt = scanTime(sql.NullString{String: firstSeenAt, Valid: true})
	summary.LastSeenAt = scanTime(sql.NullString{String: lastSeenAt, Valid: true})

	summary.Ports = []domain.Port{}
	if portsJSON != "" {
		_ = json.Unmarshal([]byte(portsJSON), &summary.Ports)
	}
	return summary, nil
}

// ContainerNameByID resolves a container id to its name.
//
// The name is the identity HarborMaster's per-container records are keyed by --
// pauses, image lineage, and update preferences all use it, because a
// recreation changes the id and the record has to survive that. This is the one
// translation between the id a caller holds and the name a record is stored
// under.
//
// Returns ErrNotFound for a container the inventory does not hold, which is
// what stops a caller writing a per-container record for something that does
// not exist.
func (r *ContainerRepository) ContainerNameByID(
	ctx context.Context,
	containerID string,
) (string, error) {
	var name string
	err := r.db.QueryRowContext(ctx,
		`SELECT name FROM containers WHERE id = ?`, containerID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read container name: %w", AsError(err))
	}
	return name, nil
}
