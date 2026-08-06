package domain

import "sort"

// Permissions and roles.
//
// # Why typed constants rather than role-name checks
//
// A handler that asks "is this user an administrator" hardcodes a policy
// decision in the place least able to see the whole picture, and spreads that
// decision across as many files as there are handlers. Adding a fourth role,
// or moving one capability between roles, then means finding every such check
// and hoping none was missed.
//
// A PERMISSION names the capability instead. Roles are defined once, in this
// file, as sets of permissions; routes declare the permission they need; and
// one middleware decides. Changing what an Operator may do is a change to one
// table, and an architecture test fails the build if a route is registered
// without an explicit access policy.
//
// # Default deny
//
// There is no permission that means "everything", and no role check that falls
// through to allow. A route with no declared policy does not serve requests --
// it fails a test at build time, and refuses at runtime if it somehow reaches
// production.

// Permission is one capability a role may hold.
//
// The vocabulary is closed and compile-time. There is no permission registry,
// no way to define one from configuration, and no wildcard.
type Permission string

// Read permissions.
//
// Held by every role. They are separated by SUBJECT rather than lumped into a
// single "read" so that a future role -- a compliance auditor who may see
// policies and audit history but not the container inventory -- can be
// expressed without redefining any of them.
const (
	// PermInventoryRead covers the inventory, containers, images, networks,
	// volumes, and their filter vocabularies.
	PermInventoryRead Permission = "inventory:read"
	// PermEventRead covers the Docker event history, the engine status, and
	// the live event stream.
	PermEventRead Permission = "event:read"
	// PermSnapshotRead covers snapshots, diffs, and restore readiness.
	PermSnapshotRead Permission = "snapshot:read"
	// PermDriftRead covers drift records and their summary.
	PermDriftRead Permission = "drift:read"
	// PermPolicyRead covers policy definitions, the rule catalogue, and
	// violations.
	PermPolicyRead Permission = "policy:read"
	// PermPlanRead covers change plans.
	PermPlanRead Permission = "plan:read"
	// PermAcquisitionRead covers image acquisition history.
	PermAcquisitionRead Permission = "acquisition:read"
	// PermExecutionRead covers container recreation history.
	PermExecutionRead Permission = "execution:read"
)

// Operator permissions.
//
// Everything that drives work or changes HarborMaster's own records, up to and
// including the two capabilities that change the Docker host.
const (
	// PermInventoryRefresh schedules a re-read of the host.
	PermInventoryRefresh Permission = "inventory:refresh"
	// PermSnapshotCreate captures a configuration snapshot.
	PermSnapshotCreate Permission = "snapshot:create"
	// PermDriftAnnotate moves a drift record's operator status. It changes
	// HarborMaster's own row and never a container.
	PermDriftAnnotate Permission = "drift:annotate"
	// PermPolicyEvaluate schedules a compliance pass.
	PermPolicyEvaluate Permission = "policy:evaluate"
	// PermPolicyAnnotate moves a violation's status.
	PermPolicyAnnotate Permission = "policy:annotate"
	// PermImageRefresh schedules a registry metadata collection pass.
	PermImageRefresh Permission = "image:refresh"
	// PermPlanGenerate schedules a plan generation pass.
	PermPlanGenerate Permission = "plan:generate"

	// PermAcquisitionCreate DOWNLOADS AN IMAGE to this host. The first of the
	// two permissions that reach a privileged socket.
	PermAcquisitionCreate Permission = "acquisition:create"
	// PermAcquisitionCancel stops a download.
	PermAcquisitionCancel Permission = "acquisition:cancel"

	// PermExecutionCreate STOPS AND REPLACES A RUNNING CONTAINER. The largest
	// permission HarborMaster has.
	PermExecutionCreate Permission = "execution:create"
	// PermExecutionCancel stops a recreation that has not yet changed anything.
	PermExecutionCancel Permission = "execution:cancel"

	// PermRollbackRead covers rollback history.
	PermRollbackRead Permission = "rollback:read"
	// PermRollbackCreate STOPS THE RUNNING REPLACEMENT and puts the preserved
	// original back. Alongside execution:create, one of the two permissions
	// that reach a root-equivalent socket and change something that is serving.
	//
	// An operator permission rather than an administrator one, deliberately.
	// The person who has to undo a bad recreation at three in the morning is
	// the person who performed it, and making them find an administrator first
	// would make the safest response the slowest one.
	PermRollbackCreate Permission = "rollback:create"
	// PermRollbackCancel stops a rollback that has not yet changed anything.
	PermRollbackCancel Permission = "rollback:cancel"
)

// Administrator permissions.
//
// Managing who may do the above, and reading the record of who did.
const (
	// PermPolicyManage creates, updates, and archives compliance rules.
	//
	// An ADMINISTRATOR permission rather than an operator one: a policy is what
	// blocks an acquisition or a recreation, so an operator able to edit
	// policies could remove the gate standing in their way.
	PermPolicyManage Permission = "policy:manage"
	// PermUserManage creates users, assigns roles, disables accounts, and
	// revokes sessions.
	PermUserManage Permission = "user:manage"
	// PermAuditRead reads the security audit history.
	PermAuditRead Permission = "audit:read"
)

// There is deliberately no `settings:manage`.
//
// HarborMaster is configured entirely through environment variables, and the
// settings page is read-only: it reports observed state and names the variables
// that produced it. A permission existed for it briefly and guarded no route,
// which is worse than no permission at all -- the role catalogue advertised a
// capability the API did not have, so a client building its UI from that
// catalogue would offer a control that could not exist.
//
// A permission is added when a route needs it, and
// TestEveryPermissionGuardsARoute fails the build on one that does not.

// AllPermissions lists every permission, sorted.
//
// Used by the role catalogue endpoint and by the test that asserts every
// permission is reachable from at least one role -- a permission no role holds
// is either a mistake or dead weight, and both are worth failing on.
var AllPermissions = []Permission{
	PermAcquisitionCancel, PermAcquisitionCreate, PermAcquisitionRead,
	PermAuditRead,
	PermDriftAnnotate, PermDriftRead,
	PermEventRead,
	PermExecutionCancel, PermExecutionCreate, PermExecutionRead,
	PermRollbackCancel, PermRollbackCreate, PermRollbackRead,
	PermImageRefresh,
	PermInventoryRead, PermInventoryRefresh,
	PermPlanGenerate, PermPlanRead,
	PermPolicyAnnotate, PermPolicyEvaluate, PermPolicyManage, PermPolicyRead,
	PermSnapshotCreate, PermSnapshotRead,
	PermUserManage,
}

// ValidPermission reports whether name is a known permission.
func ValidPermission(name string) bool {
	for _, permission := range AllPermissions {
		if string(permission) == name {
			return true
		}
	}
	return false
}

// Describe renders the permission in operator-facing words.
//
// A fixed phrase per permission, used by the role catalogue the user-admin UI
// is built from. Never assembled from input.
func (p Permission) Describe() string {
	switch p {
	case PermInventoryRead:
		return "read the container, image, network, and volume inventory"
	case PermEventRead:
		return "read Docker event history and the live event stream"
	case PermSnapshotRead:
		return "read configuration snapshots and their comparisons"
	case PermDriftRead:
		return "read configuration drift"
	case PermPolicyRead:
		return "read compliance policies and violations"
	case PermPlanRead:
		return "read change plans"
	case PermAcquisitionRead:
		return "read image acquisition history"
	case PermExecutionRead:
		return "read container recreation history"
	case PermRollbackRead:
		return "read rollback history"

	case PermInventoryRefresh:
		return "re-read this host's inventory"
	case PermSnapshotCreate:
		return "capture a configuration snapshot"
	case PermDriftAnnotate:
		return "acknowledge or classify a drift record"
	case PermPolicyEvaluate:
		return "run a compliance evaluation"
	case PermPolicyAnnotate:
		return "change a violation's status"
	case PermImageRefresh:
		return "collect image metadata from registries"
	case PermPlanGenerate:
		return "generate change plans"
	case PermAcquisitionCreate:
		return "DOWNLOAD an approved image to this host"
	case PermAcquisitionCancel:
		return "cancel an image download"
	case PermExecutionCreate:
		return "STOP AND REPLACE a running container"
	case PermExecutionCancel:
		return "cancel a container recreation"
	case PermRollbackCreate:
		return "ROLL A CONTAINER BACK to its preserved original"
	case PermRollbackCancel:
		return "cancel a rollback"

	case PermPolicyManage:
		return "create, edit, and withdraw compliance policies"
	case PermUserManage:
		return "manage users, roles, and sessions"
	case PermAuditRead:
		return "read the security audit history"
	default:
		return "an unrecognised capability"
	}
}

// Privileged reports whether a permission can change the Docker host.
//
// Exactly two do. Used to mark them in the UI and to decide which audit events
// are logged at a level a default configuration shows.
func (p Permission) Privileged() bool {
	return p == PermAcquisitionCreate || p == PermExecutionCreate ||
		p == PermRollbackCreate
}

// ---------------------------------------------------------------- roles --

// Role is a named set of permissions.
//
// Three built-in roles, and no way to define a fourth at runtime. A role
// editor would mean an authorization model an unauthenticated attacker who
// reached the port could widen, and a permission set that is data rather than
// code cannot be checked by a test.
type Role string

// The built-in roles.
const (
	// RoleViewer may read everything and change nothing.
	RoleViewer Role = "viewer"
	// RoleOperator may additionally drive work, including the two capabilities
	// that change the Docker host.
	RoleOperator Role = "operator"
	// RoleAdministrator may additionally manage users, policies, and settings,
	// and read the audit history.
	RoleAdministrator Role = "administrator"
)

// Roles lists every role, least to most privileged.
var Roles = []Role{RoleViewer, RoleOperator, RoleAdministrator}

// ValidRole reports whether name is a known role.
func ValidRole(name string) bool {
	for _, role := range Roles {
		if string(role) == name {
			return true
		}
	}
	return false
}

// Rank orders roles by privilege. Higher is more privileged.
//
// Used for display ordering and for the rule that an administrator cannot
// demote the last remaining administrator. It is NOT used for authorization:
// permission checks are set membership, not comparison, so a future role that
// is broad in one area and narrow in another cannot be accidentally granted by
// an inequality.
func (r Role) Rank() int {
	switch r {
	case RoleAdministrator:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Describe renders the role in operator-facing words.
func (r Role) Describe() string {
	switch r {
	case RoleViewer:
		return "Can read everything HarborMaster knows and change nothing."
	case RoleOperator:
		return "Everything a Viewer can do, plus running work: refreshes, " +
			"snapshots, plans, image downloads, and container recreations."
	case RoleAdministrator:
		return "Everything an Operator can do, plus managing users, policies, " +
			"settings, and the security audit history."
	default:
		return "An unrecognised role."
	}
}

// viewerPermissions is the read-only set every role holds.
var viewerPermissions = []Permission{
	PermInventoryRead,
	PermEventRead,
	PermSnapshotRead,
	PermDriftRead,
	PermPolicyRead,
	PermPlanRead,
	PermAcquisitionRead,
	PermExecutionRead,
	PermRollbackRead,
}

// operatorPermissions are what an Operator adds to the Viewer set.
var operatorPermissions = []Permission{
	PermInventoryRefresh,
	PermSnapshotCreate,
	PermDriftAnnotate,
	PermPolicyEvaluate,
	PermPolicyAnnotate,
	PermImageRefresh,
	PermPlanGenerate,
	PermAcquisitionCreate,
	PermAcquisitionCancel,
	PermExecutionCreate,
	PermRollbackCreate,
	PermRollbackCancel,
	PermExecutionCancel,
}

// administratorPermissions are what an Administrator adds to the Operator set.
var administratorPermissions = []Permission{
	PermPolicyManage,
	PermUserManage,
	PermAuditRead,
}

// rolePermissions is the whole authorization model, in one table.
//
// Built once at package initialisation rather than computed per check, and
// exposed only through Permissions and Can so no caller can mutate it.
var rolePermissions = buildRolePermissions()

func buildRolePermissions() map[Role]map[Permission]struct{} {
	viewer := permissionSet(viewerPermissions)
	operator := permissionSet(viewerPermissions, operatorPermissions)
	administrator := permissionSet(viewerPermissions, operatorPermissions, administratorPermissions)

	return map[Role]map[Permission]struct{}{
		RoleViewer:        viewer,
		RoleOperator:      operator,
		RoleAdministrator: administrator,
	}
}

func permissionSet(groups ...[]Permission) map[Permission]struct{} {
	set := make(map[Permission]struct{})
	for _, group := range groups {
		for _, permission := range group {
			set[permission] = struct{}{}
		}
	}
	return set
}

// Can reports whether the role holds the permission.
//
// The ONLY authorization primitive. An unknown role holds nothing, which is
// what makes a corrupted or future role value fail closed rather than fall
// through to a default.
func (r Role) Can(permission Permission) bool {
	if permission == "" {
		// An empty permission is a programming error -- a route policy that
		// was never filled in. Refusing is the fail-closed reading, and the
		// architecture test catches it before it can happen.
		return false
	}
	set, known := rolePermissions[r]
	if !known {
		return false
	}
	_, held := set[permission]
	return held
}

// Permissions returns the role's permissions, sorted.
//
// A fresh slice each call: the caller must not be able to reach the table.
func (r Role) Permissions() []Permission {
	set, known := rolePermissions[r]
	if !known {
		return nil
	}

	out := make([]Permission, 0, len(set))
	for permission := range set {
		out = append(out, permission)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RoleDescription is one entry in the catalogue the user-admin UI renders.
type RoleDescription struct {
	Role        Role         `json:"role"`
	Description string       `json:"description"`
	Permissions []Permission `json:"permissions"`
}

// RoleCatalogue describes every role and what it may do.
//
// Served to the UI so the role picker is built from the same source of truth
// the authorization middleware uses, rather than from a second list that can
// drift from it.
func RoleCatalogue() []RoleDescription {
	out := make([]RoleDescription, 0, len(Roles))
	for _, role := range Roles {
		out = append(out, RoleDescription{
			Role:        role,
			Description: role.Describe(),
			Permissions: role.Permissions(),
		})
	}
	return out
}
