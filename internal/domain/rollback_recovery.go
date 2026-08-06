package domain

// Manual recovery plans for a failed rollback.
//
// # Why a rollback needs its own plans
//
// A recreation that fails leaves the ORIGINAL parked and the replacement
// quarantined, and the recommended action is almost always "put the original
// back". A rollback that fails is the mirror image: the operator has already
// decided the replacement is wrong, so telling them to start it would be
// telling them to undo the decision they just made.
//
// The two also differ in what "nothing is serving" means. A recreation failing
// before its mutation point leaves the container untouched and running. A
// rollback failing after ITS mutation point has already stopped the container
// that was serving, so almost every post-mutation failure here is URGENT.
//
// # Driven by the checkpoint, never by the failure
//
// The checkpoint says what is true of the host; the failure says why
// HarborMaster stopped. Only the first can decide what to do next, and a plan
// built from the second would be guessing.
//
// # Every value here is HarborMaster's own
//
// Container ids and names come from the daemon through the normalising adapter
// or from HarborMaster's own derivation. No daemon error text, no caller input,
// and no configuration value ever reaches a plan.

// RollbackRecoveryContext is what a rollback plan is built from.
type RollbackRecoveryContext struct {
	RollbackID  string
	ExecutionID string

	// ContainerName is the production name both containers contend for.
	ContainerName string

	// OriginalID is the preserved original and ParkedName the name it was
	// parked under by the recreation. The original carries ParkedName until the
	// restore, and ContainerName afterwards.
	OriginalID string
	ParkedName string

	// ReplacementID is the container the recreation created, and
	// ReplacementParkedName the name this rollback moved it to. The latter is
	// empty until that rename happens.
	ReplacementID         string
	ReplacementParkedName string

	Checkpoint RollbackCheckpoint
	Failure    RollbackFailure

	// MutationAttempted reports that a mutation was ISSUED, whether or not any
	// checkpoint confirmed it.
	//
	// The distinction a naive design gets wrong. A checkpoint of none normally
	// means "nothing was changed". But a process killed between issuing the
	// stop and recording it also has a checkpoint of none -- and telling that
	// operator "nothing was changed" would be a confident, specific, and false
	// statement about a container that may well be down.
	MutationAttempted bool
}

// BuildRollbackRecoveryPlan describes the host after a failed rollback and
// recommends manual steps.
func BuildRollbackRecoveryPlan(context RollbackRecoveryContext) *RecoveryPlan {
	switch context.Checkpoint {
	case RollbackCheckpointNone:
		if !context.MutationAttempted {
			return rollbackUntouchedPlan(context)
		}
		return rollbackUncertainStopPlan(context)

	case RollbackCheckpointReplacementStopped:
		return rollbackStoppedPlan(context)

	case RollbackCheckpointReplacementParked:
		return rollbackParkedPlan(context)

	case RollbackCheckpointOriginalRestored:
		return rollbackRestoredPlan(context)

	case RollbackCheckpointOriginalStarted, RollbackCheckpointOriginalVerified:
		return rollbackStartedPlan(context)

	default:
		return rollbackUntouchedPlan(context)
	}
}

// rollbackUntouchedPlan is the informational case: nothing was changed.
func rollbackUntouchedPlan(context RollbackRecoveryContext) *RecoveryPlan {
	return &RecoveryPlan{
		Urgency:            RecoveryInformational,
		ServiceInterrupted: false,
		Situation: "Nothing on this host was changed. The rollback was refused or failed " +
			"before it touched a container, so the replacement is still running under " +
			shellName(context.ContainerName) + " and the original is still parked under " +
			shellName(context.ParkedName) + ".",
		Steps: []RecoveryStep{
			{Order: 1,
				Description: "No action is needed. If you still want to roll back, fix what the " +
					"refusal reported and ask again."},
		},
	}
}

// rollbackUncertainStopPlan is the interrupted-before-confirmation case.
//
// The one situation where HarborMaster genuinely does not know what it did, and
// says so rather than guessing in either direction.
func rollbackUncertainStopPlan(context RollbackRecoveryContext) *RecoveryPlan {
	steps := []RecoveryStep{
		{Order: 1,
			Description: "Check whether the replacement container is running. This is the " +
				"question HarborMaster could not answer for itself.",
			Command: "docker inspect --format '{{.State.Status}}' " + ShortenID(context.ReplacementID)},
		{Order: 2,
			Description: "If it is running, the rollback did not take effect and the host is " +
				"as it was. Ask for the rollback again, or leave it."},
		{Order: 3,
			Description: "If it is stopped, decide which container should serve. To keep the " +
				"replacement, start it again.",
			Command: "docker start " + ShortenID(context.ReplacementID)},
		{Order: 4,
			Description: "To continue the rollback by hand, rename the replacement aside, " +
				"rename the original back, and start it.",
			Command: "docker rename " + ShortenID(context.ReplacementID) + " " +
				shellName(context.ContainerName) + ".manual"},
	}
	steps = append(steps, rollbackIdentitySteps(context, len(steps))...)

	return &RecoveryPlan{
		Urgency:            RecoveryUrgent,
		ServiceInterrupted: true,
		Situation: "HarborMaster asked for the replacement container to be stopped and was " +
			"interrupted before it could confirm the result. It does not know whether " +
			shellName(context.ContainerName) + " is running. Nothing was renamed, and the " +
			"original is still parked under " + shellName(context.ParkedName) + ".",
		Steps: steps,
	}
}

// rollbackStoppedPlan covers a failure after the replacement was stopped and
// before it was renamed.
//
// Nothing is serving: the replacement holds the production name and is down,
// and the original is parked under another name.
func rollbackStoppedPlan(context RollbackRecoveryContext) *RecoveryPlan {
	steps := []RecoveryStep{
		{Order: 1,
			Description: "Decide which container should serve. Nothing is serving " +
				shellName(context.ContainerName) + " right now."},
		{Order: 2,
			Description: "To abandon the rollback and keep the replacement, start it again. " +
				"It still holds the production name.",
			Command: "docker start " + ShortenID(context.ReplacementID)},
		{Order: 3,
			Description: "To finish the rollback by hand, first move the replacement out of " +
				"the way so the name comes free.",
			Command: "docker rename " + ShortenID(context.ReplacementID) + " " +
				shellName(context.ContainerName) + ".manual"},
		{Order: 4,
			Description: "Then give the original its name back.",
			Command: "docker rename " + ShortenID(context.OriginalID) + " " +
				shellName(context.ContainerName)},
		{Order: 5,
			Description: "Then start the original.",
			Command:     "docker start " + ShortenID(context.OriginalID)},
	}
	steps = append(steps, rollbackIdentitySteps(context, len(steps))...)

	return &RecoveryPlan{
		Urgency:            RecoveryUrgent,
		ServiceInterrupted: true,
		Situation: "The replacement container was stopped and still carries the name " +
			shellName(context.ContainerName) + ". The original is stopped and parked under " +
			shellName(context.ParkedName) + ". Nothing is serving this name.",
		Steps: steps,
	}
}

// rollbackParkedPlan covers a failure after the replacement was renamed aside
// and before the original took the name back.
//
// The most awkward state: the production name is held by nobody.
func rollbackParkedPlan(context RollbackRecoveryContext) *RecoveryPlan {
	steps := []RecoveryStep{
		{Order: 1,
			Description: "No container currently holds " + shellName(context.ContainerName) +
				". Two containers are stopped and each carries a parked name."},
		{Order: 2,
			Description: "To finish the rollback by hand, give the original its name back.",
			Command: "docker rename " + ShortenID(context.OriginalID) + " " +
				shellName(context.ContainerName)},
		{Order: 3,
			Description: "Then start the original.",
			Command:     "docker start " + ShortenID(context.OriginalID)},
		{Order: 4,
			Description: "To abandon the rollback instead, give the name back to the " +
				"replacement and start that.",
			Command: "docker rename " + ShortenID(context.ReplacementID) + " " +
				shellName(context.ContainerName)},
	}
	steps = append(steps, rollbackIdentitySteps(context, len(steps))...)

	return &RecoveryPlan{
		Urgency:            RecoveryUrgent,
		ServiceInterrupted: true,
		Situation: "The replacement container was stopped and renamed to " +
			shellName(context.ReplacementParkedName) + ", and the original has not yet been " +
			"renamed back. No container holds " + shellName(context.ContainerName) +
			", and nothing is serving it.",
		Steps: steps,
	}
}

// rollbackRestoredPlan covers a failure after the original took its name back
// and before it started.
func rollbackRestoredPlan(context RollbackRecoveryContext) *RecoveryPlan {
	steps := []RecoveryStep{
		{Order: 1,
			Description: "The original holds its own name again but is not running. Start it.",
			Command:     "docker start " + ShortenID(context.OriginalID)},
		{Order: 2,
			Description: "If it will not start, read its logs. It ran under this configuration " +
				"before the recreation, so a failure now is new information.",
			Command: "docker logs --tail 100 " + ShortenID(context.OriginalID)},
		{Order: 3,
			Description: "The replacement is stopped and parked under " +
				shellName(context.ReplacementParkedName) + ". It is kept as evidence and is " +
				"safe to leave."},
	}
	steps = append(steps, rollbackIdentitySteps(context, len(steps))...)

	return &RecoveryPlan{
		Urgency:            RecoveryUrgent,
		ServiceInterrupted: true,
		Situation: "The original container carries the name " + shellName(context.ContainerName) +
			" again but is not running. The replacement is stopped and parked under " +
			shellName(context.ReplacementParkedName) + ". Nothing is serving this name.",
		Steps: steps,
	}
}

// rollbackStartedPlan covers a failure after the original was started.
//
// The original is RUNNING, so the service is probably back. What failed is a
// proof, and that is a materially different situation from the ones above:
// something is serving, and the question is whether it is serving correctly.
func rollbackStartedPlan(context RollbackRecoveryContext) *RecoveryPlan {
	steps := []RecoveryStep{
		{Order: 1,
			Description: "The original is running under its own name. Check whether it is " +
				"actually serving before doing anything else.",
			Command: "docker inspect --format '{{.State.Status}} {{.State.Health.Status}}' " +
				ShortenID(context.OriginalID)},
		{Order: 2,
			Description: "If it is not healthy, read its logs.",
			Command:     "docker logs --tail 100 " + ShortenID(context.OriginalID)},
		{Order: 3,
			Description: "The replacement is stopped and parked under " +
				shellName(context.ReplacementParkedName) + ". It is kept as evidence of why " +
				"the recreation was backed out, and is safe to leave."},
		{Order: 4,
			Description: "Remove the parked replacement only once you are satisfied the " +
				"original is serving correctly.",
			Command:     "docker rm " + ShortenID(context.ReplacementID),
			Destructive: true},
	}
	steps = append(steps, rollbackIdentitySteps(context, len(steps))...)

	return &RecoveryPlan{
		Urgency: RecoveryAttention,
		// The original is running, so the service is most likely restored. The
		// proof did not pass, which is why this needs a person -- but claiming
		// the service is down would send an operator running at the wrong
		// problem.
		ServiceInterrupted: false,
		Situation: "The original container is running under " + shellName(context.ContainerName) +
			", but a verification did not pass. The replacement is stopped and parked under " +
			shellName(context.ReplacementParkedName) + ".",
		Steps: steps,
	}
}

// rollbackIdentitySteps appends both container identities as inspectable facts.
//
// Ids rather than names, because a failure during a rename is precisely the
// case where the names are not what anyone expects.
func rollbackIdentitySteps(context RollbackRecoveryContext, offset int) []RecoveryStep {
	steps := make([]RecoveryStep, 0, 2)

	if context.OriginalID != "" {
		steps = append(steps, RecoveryStep{
			Order: offset + len(steps) + 1,
			Description: "The original container's id is " + ShortenID(context.OriginalID) +
				". Names can be reorganised; this cannot.",
			Command: "docker inspect " + ShortenID(context.OriginalID),
		})
	}
	if context.ReplacementID != "" {
		steps = append(steps, RecoveryStep{
			Order:       offset + len(steps) + 1,
			Description: "The replacement container's id is " + ShortenID(context.ReplacementID) + ".",
			Command:     "docker inspect " + ShortenID(context.ReplacementID),
		})
	}
	return steps
}
