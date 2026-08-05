package domain

import "strings"

// Drift classification: turning one DiffEntry into a category and a severity.
//
// # Why this lives in domain and is pure
//
// Classification is policy, not I/O. Keeping it as a total function of
// (group, field, kind, old, new) means it can be exhaustively tested without a
// database, a Docker daemon, or a diff engine, and means two callers can never
// disagree about how severe something is.
//
// # The ordering principle
//
// Severity answers one question: HOW MUCH WORSE IS THE HOST NOW? Not "how
// surprising is this", not "how large is the change". So the ranking follows
// the blast radius of the difference, and direction matters more than the
// field does:
//
//   - CRITICAL is a lost containment boundary. Privileged mode on, a read-only
//     root filesystem made writable, a capability added, a security option
//     removed. Also an image digest change, because the container is running
//     code nobody recorded.
//   - HIGH widens the attack surface or removes a safety net without breaking
//     containment: a new image reference, a health check removed, a bind mount
//     added, a port published.
//   - MEDIUM changes behaviour in a way that could matter: environment,
//     restart policy, a memory limit removed, network membership.
//   - LOW is bookkeeping: labels, a CPU limit, a health-check interval,
//     Compose metadata.
//
// DIRECTION IS THE POINT. `privileged` moving false→true is critical;
// true→false is the operator fixing something and is low. A capability ADDED is
// critical; one dropped is not. Ranking the field rather than the movement
// would fill an operator's dashboard with critical alerts for improvements,
// and a dashboard that cries wolf gets ignored -- which costs more than having
// no dashboard at all.

// DriftClassification is the verdict for one difference.
type DriftClassification struct {
	Category DriftCategory
	Severity DriftSeverity
	// Reason is short prose explaining the ranking, from the fixed set written
	// in this file. It is never derived from a value, so it can never carry
	// one.
	Reason string
}

// ClassifyDrift ranks one difference.
//
// group and field come from the diff engine, kind from its comparison, and old
// and new are the rendered values -- EMPTY for a sensitive field, which is why
// no branch here may depend on them being present.
func ClassifyDrift(group DiffGroupName, field string, kind ChangeKind, old, new string) DriftClassification {
	category := driftCategory(group, field)

	// A comparison that could not be made is not a change. It is reported so
	// an operator knows the field was not verified, at the severity of the
	// thing that could not be checked rather than at "nothing happened".
	if kind == ChangeUnverifiable {
		return DriftClassification{
			Category: category,
			Severity: DriftSeverityMedium,
			Reason:   "the values could not be compared, so this field is unverified",
		}
	}

	if verdict, ok := classifySecurity(group, field, kind, old, new); ok {
		verdict.Category = category
		return verdict
	}
	if verdict, ok := classifyImage(group, field, kind); ok {
		verdict.Category = category
		return verdict
	}
	if verdict, ok := classifyMounts(group, field, kind, old, new); ok {
		verdict.Category = category
		return verdict
	}
	if verdict, ok := classifyPorts(group, field, kind, old, new); ok {
		verdict.Category = category
		return verdict
	}
	if verdict, ok := classifyHealth(group, field, kind); ok {
		verdict.Category = category
		return verdict
	}
	if verdict, ok := classifyResources(group, field, kind); ok {
		verdict.Category = category
		return verdict
	}

	return DriftClassification{
		Category: category,
		Severity: defaultSeverity(category),
		Reason:   defaultReason(category),
	}
}

// classifySecurity ranks the containment boundary.
//
// Every branch is direction-aware. Losing a boundary is critical; regaining
// one is an improvement and ranks low.
func classifySecurity(group DiffGroupName, field string, kind ChangeKind, old, new string) (DriftClassification, bool) {
	if group != DiffGroupSecurity {
		return DriftClassification{}, false
	}

	switch field {
	case "privileged":
		if becameTrue(kind, old, new) {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "the container now runs privileged, which is equivalent to root on the host",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "privileged mode was turned off, which strengthens containment",
		}, true

	case "readonlyRootfs":
		if becameFalse(kind, old, new) {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "the root filesystem is no longer read-only, so the running image can be modified in place",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "the root filesystem was made read-only, which strengthens containment",
		}, true

	case "noNewPrivileges":
		if becameFalse(kind, old, new) {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "no-new-privileges was disabled, so a setuid binary can now escalate",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "no-new-privileges was enabled, which strengthens containment",
		}, true

	case "capAdd":
		// A set rendered as a sorted, comma-joined string. Growth means a
		// capability was granted; shrinkage means one was given up.
		if setGrew(kind, old, new) {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "a Linux capability was added, widening what the container may do to the host",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "a Linux capability was removed, which strengthens containment",
		}, true

	case "capDrop":
		// Dropping MORE is safer; dropping fewer means a capability came back.
		if setShrank(kind, old, new) {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "a dropped Linux capability was restored, widening what the container may do",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "more Linux capabilities are dropped, which strengthens containment",
		}, true

	case "securityOpt":
		if setShrank(kind, old, new) || kind == ChangeRemoved {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "a security option was removed, weakening the container's confinement profile",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "the security option set changed",
		}, true

	case "apparmorProfile", "seccompProfile", "selinuxLabel":
		if kind == ChangeRemoved || strings.TrimSpace(new) == "" {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "a mandatory access control profile was removed, weakening confinement",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a mandatory access control profile changed",
		}, true
	}

	// Namespace modes: sharing the host's namespace defeats isolation.
	if field == "pidMode" || field == "ipcMode" || field == "utsMode" || field == "usernsMode" {
		if strings.Contains(new, "host") {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "the container now shares a host namespace, which defeats isolation",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a namespace mode changed",
		}, true
	}

	if strings.HasPrefix(field, "device.") {
		if kind == ChangeAdded {
			return DriftClassification{
				Severity: DriftSeverityCritical,
				Reason:   "a host device was exposed to the container",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a host device mapping changed",
		}, true
	}

	if strings.HasPrefix(field, "sysctl.") {
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a kernel parameter override changed",
		}, true
	}

	return DriftClassification{
		Severity: DriftSeverityHigh,
		Reason:   "a security setting changed",
	}, true
}

// classifyImage ranks image identity.
//
// The DIGEST outranks the reference. A tag is a name and can be repointed
// without anything being rebuilt; a digest is the content itself, so a digest
// change means the container is running different bytes than the ones
// recorded -- which is the fact that matters.
func classifyImage(group DiffGroupName, field string, kind ChangeKind) (DriftClassification, bool) {
	if group != DiffGroupMetadata {
		return DriftClassification{}, false
	}

	switch field {
	case "image.digest", "image.id":
		return DriftClassification{
			Severity: DriftSeverityCritical,
			Reason:   "the image content changed, so the container is running code the baseline does not describe",
		}, true
	case "image.reference":
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "the image reference changed",
		}, true
	case "process.command", "process.entrypoint":
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "the process the container runs changed",
		}, true
	case "process.user":
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "the user the container runs as changed",
		}, true
	}
	_ = kind
	return DriftClassification{}, false
}

// classifyMounts ranks filesystem exposure.
//
// A BIND mount reaches the host filesystem; a named volume does not. That
// distinction is the whole ranking.
func classifyMounts(group DiffGroupName, field string, kind ChangeKind, old, new string) (DriftClassification, bool) {
	if group != DiffGroupMounts {
		return DriftClassification{}, false
	}
	_ = field

	// mountMap renders a mount as "<type> source=... [ro]", so the type is the
	// prefix and read-only is a suffix flag.
	bind := strings.HasPrefix(new, string(MountTypeBind))
	wasReadOnly := strings.HasSuffix(old, " ro")
	isReadOnly := strings.HasSuffix(new, " ro")

	switch {
	case kind == ChangeAdded && bind:
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a bind mount was added, exposing a host path to the container",
		}, true

	case kind == ChangeAdded:
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a mount was added",
		}, true

	// Losing the read-only flag creates a write path into the mount that did
	// not exist before, which is why direction is checked rather than just
	// reporting "modified".
	case kind == ChangeModified && wasReadOnly && !isReadOnly:
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a mount is no longer read-only, creating a write path that did not exist",
		}, true

	default:
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a mount changed",
		}, true
	}
}

// classifyPorts ranks network exposure.
//
// PUBLISHED is what matters. An exposed port is documentation; a published one
// is reachable from outside the host's container network.
func classifyPorts(group DiffGroupName, field string, kind ChangeKind, old, new string) (DriftClassification, bool) {
	if group != DiffGroupPorts {
		return DriftClassification{}, false
	}
	_ = field

	published := strings.HasPrefix(new, "published")
	wasPublished := strings.HasPrefix(old, "published")

	if kind == ChangeAdded && published {
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a published port was added, making the container reachable from outside the host",
		}, true
	}
	if kind == ChangeModified && published && !wasPublished {
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "a port that was only exposed is now published",
		}, true
	}
	if kind == ChangeAdded {
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a port was exposed",
		}, true
	}
	return DriftClassification{
		Severity: DriftSeverityMedium,
		Reason:   "a port mapping changed",
	}, true
}

// classifyHealth ranks the health check.
//
// Removing the check outranks changing its timing: a container with no health
// check reports healthy forever, which is worse than one that checks slowly.
func classifyHealth(group DiffGroupName, field string, kind ChangeKind) (DriftClassification, bool) {
	if group != DiffGroupMetadata || !strings.HasPrefix(field, "healthCheck.") {
		return DriftClassification{}, false
	}

	switch {
	case kind == ChangeRemoved && field == "healthCheck.test":
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "the health check was removed, so the container will report healthy regardless of its state",
		}, true
	case field == "healthCheck.disabled":
		return DriftClassification{
			Severity: DriftSeverityHigh,
			Reason:   "the health check was disabled",
		}, true
	case field == "healthCheck.test":
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "the health check command changed",
		}, true
	default:
		// Interval, timeout, retries: timing, not presence.
		return DriftClassification{
			Severity: DriftSeverityLow,
			Reason:   "a health check timing parameter changed",
		}, true
	}
}

// classifyResources ranks limits.
//
// A REMOVED memory limit is the one that matters: an unbounded container can
// take the host down through the OOM killer, which is a availability problem
// for every other container on it. A CPU limit is throttling, not a ceiling on
// damage.
func classifyResources(group DiffGroupName, field string, kind ChangeKind) (DriftClassification, bool) {
	if group != DiffGroupResources {
		return DriftClassification{}, false
	}

	memory := strings.HasPrefix(field, "memory") || field == "pidsLimit"
	if memory {
		if kind == ChangeRemoved {
			return DriftClassification{
				Severity: DriftSeverityMedium,
				Reason:   "a memory or process limit was removed, so the container can now exhaust host resources",
			}, true
		}
		return DriftClassification{
			Severity: DriftSeverityMedium,
			Reason:   "a memory or process limit changed",
		}, true
	}

	return DriftClassification{
		Severity: DriftSeverityLow,
		Reason:   "a CPU or I/O resource setting changed",
	}, true
}

// driftCategory maps a diff group and field onto a drift category.
//
// The metadata group is split, because the diff engine puts image identity,
// the process, the restart policy, the health check and logging in one bucket
// and an operator thinks about them separately.
func driftCategory(group DiffGroupName, field string) DriftCategory {
	switch group {
	case DiffGroupEnvironment:
		return DriftCategoryEnvironment
	case DiffGroupLabels:
		return DriftCategoryLabels
	case DiffGroupPorts:
		return DriftCategoryPorts
	case DiffGroupNetworks:
		return DriftCategoryNetworks
	case DiffGroupMounts:
		return DriftCategoryMounts
	case DiffGroupResources:
		return DriftCategoryResources
	case DiffGroupSecurity:
		return DriftCategorySecurity
	case DiffGroupCompose:
		return DriftCategoryCompose
	}

	switch {
	case strings.HasPrefix(field, "image."):
		return DriftCategoryImage
	case strings.HasPrefix(field, "process."):
		return DriftCategoryProcess
	case strings.HasPrefix(field, "healthCheck."):
		return DriftCategoryHealth
	case strings.HasPrefix(field, "logging."):
		return DriftCategoryLogging
	case strings.HasPrefix(field, "restartPolicy"):
		return DriftCategoryRestart
	default:
		return DriftCategoryMetadata
	}
}

// defaultSeverity is the ranking for a category with no specific rule.
func defaultSeverity(category DriftCategory) DriftSeverity {
	switch category {
	case DriftCategorySecurity, DriftCategoryImage:
		return DriftSeverityHigh
	case DriftCategoryEnvironment, DriftCategoryNetworks, DriftCategoryRestart,
		DriftCategoryProcess, DriftCategoryMounts, DriftCategoryPorts:
		return DriftSeverityMedium
	case DriftCategoryLabels, DriftCategoryCompose, DriftCategoryResources,
		DriftCategoryLogging, DriftCategoryHealth, DriftCategoryMetadata:
		return DriftSeverityLow
	default:
		return DriftSeverityLow
	}
}

// defaultReason is the prose for a category with no specific rule.
func defaultReason(category DriftCategory) string {
	switch category {
	case DriftCategoryEnvironment:
		return "an environment variable changed"
	case DriftCategoryLabels:
		return "a label changed"
	case DriftCategoryNetworks:
		return "network membership changed"
	case DriftCategoryRestart:
		return "the restart policy changed"
	case DriftCategoryLogging:
		return "the logging configuration changed"
	case DriftCategoryCompose:
		return "Compose metadata changed"
	case DriftCategoryProcess:
		return "the process configuration changed"
	case DriftCategoryImage:
		return "image metadata changed"
	default:
		return "the configuration changed"
	}
}

// --- direction helpers -------------------------------------------------------
//
// Each answers "did this get worse?" rather than "did this change?". They take
// the rendered values, which are booleans or sorted comma-joined sets as the
// diff engine's projections produce them.

// becameTrue reports whether a boolean field turned on.
func becameTrue(kind ChangeKind, old, new string) bool {
	if kind == ChangeAdded {
		return new == "true"
	}
	return old == "false" && new == "true"
}

// becameFalse reports whether a boolean field turned off.
func becameFalse(kind ChangeKind, old, new string) bool {
	if kind == ChangeRemoved {
		return old == "true"
	}
	return old == "true" && new == "false"
}

// setGrew reports whether a comma-joined set gained a member.
func setGrew(kind ChangeKind, old, new string) bool {
	if kind == ChangeAdded {
		return countMembers(new) > 0
	}
	if kind == ChangeRemoved {
		return false
	}
	return countMembers(new) > countMembers(old)
}

// setShrank reports whether a comma-joined set lost a member.
func setShrank(kind ChangeKind, old, new string) bool {
	if kind == ChangeRemoved {
		return countMembers(old) > 0
	}
	if kind == ChangeAdded {
		return false
	}
	return countMembers(new) < countMembers(old)
}

// countMembers counts entries in a comma-joined set, treating empty as zero
// rather than as one empty member.
func countMembers(value string) int {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	return strings.Count(value, ",") + 1
}
