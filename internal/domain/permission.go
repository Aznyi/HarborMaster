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
	// PermAutomationRead covers update policies, scheduler passes, decisions,
	// and paused containers.
	//
	// A READ permission every role holds, deliberately. Automation is the one
	// subsystem that changes the host with nobody watching, and a viewer who
	// cannot see what it decided cannot answer the question the role exists to
	// let them answer.
	PermAutomationRead Permission = "automation:read"
	// PermDependencyRead covers workload dependencies: what must be stable
	// before something else changes, the graph, and the update order.
	//
	// A READ permission every role holds, for the same reason automation:read
	// is. A dependency relationship is why an update did not happen, and a
	// viewer who cannot see one cannot answer "why is that container still on
	// the old image".
	//
	// Reading a relationship reveals two container names and which namespace
	// they share. It reveals no image, no digest, no environment value, and no
	// configuration.
	PermDependencyRead Permission = "dependency:read"
	// PermNotificationRead covers notification destinations, rules, and the
	// delivery history.
	//
	// A READ permission every role holds. The delivery history is how an
	// operator answers "was anybody told about this", and a destination's
	// public record carries no credential -- the URL and the relay password
	// live in a separate type this permission cannot reach at all.
	PermNotificationRead Permission = "notification:read"
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
	// PermPlanApprove records that a person reviewed a change plan the planner
	// asked a human to look at.
	//
	// # Why this is an operator permission, and not an administrator one
	//
	// An operator already holds execution:create and automation:approve -- they
	// may recreate a container by hand, and they may release a decision a policy
	// held. Plan approval is NARROWER than either: one human, one immutable
	// plan, one digest, no standing authority over anything else.
	//
	// The administrator permissions are the ones that grant STANDING authority
	// -- writing an update policy is dangerous precisely because it is an
	// unattended version of execution:create. A one-off judgement about one
	// assessment is not that.
	//
	// # Why it is separate from execution:create
	//
	// Approving a plan and applying it are two acts, and a deployment may want
	// to keep them apart. Holding this permission grants no ability to change a
	// container: the apply still needs acquisition:create and execution:create,
	// and every preflight still runs.
	PermPlanApprove Permission = "plan:approve"

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

	// PermAutomationRun starts a scheduler pass by hand, or previews one as a
	// dry run.
	//
	// An operator permission because a manual pass submits exactly the work the
	// scheduled pass would have submitted a few minutes later -- it changes
	// WHEN, not WHETHER. What may be updated at all is the policy's business,
	// and editing a policy is an administrator permission.
	PermAutomationRun Permission = "automation:run"
	// PermAutomationApprove releases a decision a policy held for a person.
	//
	// The whole point of approval mode is that a human commits, so the
	// permission belongs to the role that would have performed the update by
	// hand.
	PermAutomationApprove Permission = "automation:approve"
	// PermAutomationPause stops automation for one container, and resumes it.
	//
	// Pausing is a safety action anyone who can operate the host should be able
	// to take immediately. Resuming is the same permission: an operator who
	// investigated the failure is the person who should clear it, and requiring
	// an administrator would make the safe state the inconvenient one.
	PermAutomationPause Permission = "automation:pause"
)

// Administrator permissions.
//
// Managing who may do the above, and reading the record of who did.
const (
	// PermAutomationManage creates, edits, and withdraws UPDATE policies.
	//
	// The most consequential permission in HarborMaster, and an administrator
	// one for the reason PermPolicyManage is: an update policy decides what the
	// host may do to itself unattended. An operator who could write one could
	// grant themselves a standing, unattended version of execution:create over
	// any container a selector reaches.
	PermAutomationManage Permission = "automation:manage"

	// PermDependencyManage creates and removes OPERATOR-defined ordering
	// relationships.
	//
	// # Administrator-only, and the reason is the DELETE
	//
	// Creating a relationship can only ever make HarborMaster wait or refuse --
	// it is a safety constraint, and adding one is conservative. Removing one
	// takes a safety constraint away, and it is the same permission.
	//
	// So this sits with automation:manage rather than with the operator
	// capabilities: an operator able to delete an ordering relationship could
	// clear the gate standing between an unattended update and a container that
	// depends on something else finishing first. That is the same reasoning
	// that puts policy:manage and automation:manage here.
	//
	// It grants nothing over Docker. A relationship cannot start a pull, a
	// recreation, or a rollback, and there is no field on the create request
	// for an image, a digest, or a container id.
	//
	// It cannot touch a DISCOVERED relationship. Those are derived from the
	// inventory on every read and have no stored row to delete.
	PermDependencyManage Permission = "dependency:manage"
	// PermPolicyManage creates, updates, and archives compliance rules.
	//
	// An ADMINISTRATOR permission rather than an operator one: a policy is what
	// blocks an acquisition or a recreation, so an operator able to edit
	// policies could remove the gate standing in their way.
	PermPolicyManage Permission = "policy:manage"
	// PermUserManage creates users, assigns roles, disables accounts, and
	// revokes sessions.
	// PermNotificationManage creates, edits, and archives notification
	// destinations and rules, and sends a test notification.
	//
	// An ADMINISTRATOR permission because a destination CARRIES A CREDENTIAL --
	// a Slack, Discord, or Teams webhook URL is a bearer token in the shape of
	// a path -- and because creating one points HarborMaster's second outbound
	// egress somewhere new. An operator able to create a destination could
	// exfiltrate every container name and update event this host produces to a
	// server they control.
	PermNotificationManage Permission = "notification:manage"
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
	PermAutomationApprove, PermAutomationManage, PermAutomationPause,
	PermAutomationRead, PermAutomationRun,
	PermDependencyManage, PermDependencyRead,
	PermDriftAnnotate, PermDriftRead,
	PermEventRead,
	PermExecutionCancel, PermExecutionCreate, PermExecutionRead,
	PermRollbackCancel, PermRollbackCreate, PermRollbackRead,
	PermImageRefresh,
	PermInventoryRead, PermInventoryRefresh,
	PermNotificationManage, PermNotificationRead,
	PermPlanApprove, PermPlanGenerate, PermPlanRead,
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
	case PermNotificationRead:
		return "read notification destinations, rules, and delivery history"
	case PermNotificationManage:
		return "manage notification destinations, rules, and credentials"
	case PermRollbackRead:
		return "read rollback history"
	case PermAutomationRead:
		return "read update policies, automation passes, and paused containers"
	case PermDependencyRead:
		return "read which containers must be stable before others can be updated"
	case PermDependencyManage:
		return "record and REMOVE ordering relationships between containers"

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
	case PermAutomationRun:
		return "run an automation pass now, or preview one"
	case PermAutomationApprove:
		return "APPROVE an automation decision, applying the update"
	case PermAutomationPause:
		return "pause and resume automation for a container"

	case PermAutomationManage:
		return "create, edit, and withdraw UPDATE POLICIES, which let HarborMaster " +
			"change containers unattended"
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
// Four do: the three that reach a socket directly, plus approving an automation
// decision, which submits an acquisition that will lead to a recreation. Used to
// mark them in the UI and to decide which audit events are logged at a level a
// default configuration shows.
func (p Permission) Privileged() bool {
	return p == PermAcquisitionCreate || p == PermExecutionCreate ||
		p == PermRollbackCreate || p == PermAutomationApprove
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
	PermAutomationRead,
	PermNotificationRead,
	PermDependencyRead,
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
	PermPlanApprove,
	PermAutomationRun,
	PermAutomationApprove,
	PermAutomationPause,
}

// administratorPermissions are what an Administrator adds to the Operator set.
var administratorPermissions = []Permission{
	PermAutomationManage,
	// Removing an ordering relationship removes a safety constraint. See the
	// note on PermDependencyManage.
	PermDependencyManage,
	PermNotificationManage,
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
