package service

import (
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Event classification: what an event means for the inventory.
//
// This is application policy, which is why it lives here rather than in
// internal/docker. The adapter translates a protocol; this decides what to do
// about it.
//
// The governing rule is that an event is a HINT. Classification never says what
// a container now looks like -- it says which resource to go and re-read. A
// `start` event and a `die` event both produce "re-inspect this container",
// because the answer to "what state is it in" comes from Docker, not from the
// event's name.

// containerRefreshActions are the container actions worth re-inspecting for.
//
// An allowlist rather than a denylist: a Docker version that adds a new action
// should not silently start triggering refreshes on a burst of something
// HarborMaster has never seen. Unknown actions are recorded and ignored, which
// is visible in the history and costs nothing.
var containerRefreshActions = map[domain.DockerEventAction]struct{}{
	domain.ActionCreate:       {},
	domain.ActionStart:        {},
	domain.ActionStop:         {},
	domain.ActionDie:          {},
	domain.ActionKill:         {},
	domain.ActionPause:        {},
	domain.ActionUnpause:      {},
	domain.ActionRestart:      {},
	domain.ActionRename:       {},
	domain.ActionUpdate:       {},
	domain.ActionHealthStatus: {},
	domain.ActionOOM:          {},
	domain.ActionAttach:       {},
	domain.ActionDetach:       {},
	// connect/disconnect on a container are network attachment changes, which
	// change the container's own network list.
	domain.ActionConnect:    {},
	domain.ActionDisconnect: {},
}

// imageRefreshActions name a single image whose metadata can be re-read.
var imageRefreshActions = map[domain.DockerEventAction]struct{}{
	domain.ActionPull:  {},
	domain.ActionTag:   {},
	domain.ActionUntag: {},
}

// imageCatalogActions affect images in bulk or remove one outright, so there is
// nothing single to inspect.
//
// A delete cannot be resolved by inspecting the deleted image, and a prune
// names nothing at all. Both escalate to a bounded catalog pass rather than a
// full inventory reconciliation: image churn is common on a build host, and
// making every `docker rmi` sweep a thousand containers would be worse than the
// problem it solves.
var imageCatalogActions = map[domain.DockerEventAction]struct{}{
	domain.ActionDelete: {},
	domain.ActionPrune:  {},
}

// networkRefreshActions change network metadata or attachments.
var networkRefreshActions = map[domain.DockerEventAction]struct{}{
	domain.ActionCreate:     {},
	domain.ActionDestroy:    {},
	domain.ActionUpdate:     {},
	domain.ActionConnect:    {},
	domain.ActionDisconnect: {},
	domain.ActionRemove:     {},
	domain.ActionPrune:      {},
}

// volumeRefreshActions change volume metadata or mount state.
var volumeRefreshActions = map[domain.DockerEventAction]struct{}{
	domain.ActionCreate:  {},
	domain.ActionDestroy: {},
	domain.ActionMount:   {},
	domain.ActionUnmount: {},
	domain.ActionRemove:  {},
	domain.ActionPrune:   {},
}

// Classification is what an event asks the inventory to do.
type Classification struct {
	// Request is the synchronization kind.
	Request domain.RefreshRequest
	// Target identifies the resource: a container ID, an image reference, and
	// so on. Empty for catalog-wide and full requests.
	Target string
	// Result is the processing outcome to record on the event row.
	Result domain.EventProcessingResult
	// Reason is a short, operator-facing explanation recorded when the result
	// is a warning. Never a raw error.
	Reason string
}

// ClassifyEvent decides what one event asks for.
//
// Exported so its table of cases is directly testable: the mapping from event
// to action is the part of the engine most likely to be wrong in a way nothing
// else would catch.
func ClassifyEvent(event domain.DockerEvent) Classification {
	switch event.Type {
	case domain.EventTypeContainer:
		return classifyContainer(event)
	case domain.EventTypeImage:
		return classifyImage(event)
	case domain.EventTypeNetwork:
		return classifyNetwork(event)
	case domain.EventTypeVolume:
		return classifyVolume(event)
	case domain.EventTypeDaemon:
		return classifyDaemon(event)
	default:
		// An event type HarborMaster does not model. Recorded so the history is
		// honest about what arrived, but it triggers nothing: escalating an
		// unrecognised type to a full sweep would let a chatty daemon feature
		// HarborMaster does not use drive its refresh schedule.
		return Classification{
			Request: domain.RefreshNone,
			Result:  domain.ResultIgnored,
		}
	}
}

func classifyContainer(event domain.DockerEvent) Classification {
	// destroy is terminal and unambiguous: there is nothing left to inspect, so
	// this is the one case that writes a conclusion rather than requesting a
	// read. It writes only "no longer present", which no inspection could ever
	// return, and never a state or a configuration.
	if event.Action == domain.ActionDestroy || event.Action == domain.ActionRemove {
		if event.ActorID == "" {
			return unmappable("a container was destroyed but the event named no container")
		}
		return Classification{
			Request: domain.RefreshContainerAbsent,
			Target:  event.ActorID,
			Result:  domain.ResultProcessed,
		}
	}

	if _, wanted := containerRefreshActions[event.Action]; !wanted {
		// exec_create, exec_start, exec_die, top, copy, export, archive-path
		// and friends. Real events, no inventory consequence.
		return Classification{Request: domain.RefreshNone, Result: domain.ResultIgnored}
	}

	// A container event with no actor ID cannot be targeted. Rather than guess,
	// escalate: a full reconciliation is expensive but correct, and this is
	// rare enough that the cost does not matter.
	if event.ActorID == "" {
		return unmappable("a container event named no container")
	}

	return Classification{
		Request: domain.RefreshContainer,
		Target:  event.ActorID,
		Result:  domain.ResultProcessed,
	}
}

func classifyImage(event domain.DockerEvent) Classification {
	if _, catalog := imageCatalogActions[event.Action]; catalog {
		return Classification{
			Request: domain.RefreshImageCatalog,
			Result:  domain.ResultProcessed,
		}
	}

	if _, wanted := imageRefreshActions[event.Action]; !wanted {
		return Classification{Request: domain.RefreshNone, Result: domain.ResultIgnored}
	}

	// An image event's actor is an ID for a pull and a reference for a tag.
	// Both are valid arguments to InspectImage, so either works as the target.
	target := event.ActorID
	if target == "" {
		target = strings.TrimSpace(event.Attributes["name"])
	}
	if target == "" {
		return Classification{
			Request: domain.RefreshImageCatalog,
			Result:  domain.ResultProcessed,
		}
	}

	return Classification{
		Request: domain.RefreshImage,
		Target:  target,
		Result:  domain.ResultProcessed,
	}
}

func classifyNetwork(event domain.DockerEvent) Classification {
	if _, wanted := networkRefreshActions[event.Action]; !wanted {
		return Classification{Request: domain.RefreshNone, Result: domain.ResultIgnored}
	}

	// A network connect or disconnect changes the ATTACHED CONTAINER's network
	// list as well as the network itself, and the container's list is what the
	// UI actually renders. When the event names the container, refresh that
	// container: it is the more specific and more useful of the two, and the
	// network catalog is re-read by the periodic reconciliation anyway.
	if event.Action == domain.ActionConnect || event.Action == domain.ActionDisconnect {
		if containerID := strings.TrimSpace(event.Attributes["container"]); containerID != "" {
			return Classification{
				Request: domain.RefreshContainer,
				Target:  containerID,
				Result:  domain.ResultProcessed,
			}
		}
	}

	return Classification{Request: domain.RefreshNetworks, Result: domain.ResultProcessed}
}

func classifyVolume(event domain.DockerEvent) Classification {
	if _, wanted := volumeRefreshActions[event.Action]; !wanted {
		return Classification{Request: domain.RefreshNone, Result: domain.ResultIgnored}
	}

	// As with networks: a mount or unmount changes the container's mount list,
	// which is the record an operator reads.
	if event.Action == domain.ActionMount || event.Action == domain.ActionUnmount {
		if containerID := strings.TrimSpace(event.Attributes["container"]); containerID != "" {
			return Classification{
				Request: domain.RefreshContainer,
				Target:  containerID,
				Result:  domain.ResultProcessed,
			}
		}
	}

	return Classification{Request: domain.RefreshVolumes, Result: domain.ResultProcessed}
}

func classifyDaemon(event domain.DockerEvent) Classification {
	// A daemon reload means its configuration changed underneath everything
	// HarborMaster has recorded. Nothing narrower than a full sweep is honest.
	if event.Action == domain.ActionReload {
		return Classification{
			Request: domain.RefreshFull,
			Result:  domain.ResultProcessed,
			Reason:  "the docker daemon reloaded its configuration",
		}
	}
	return Classification{Request: domain.RefreshNone, Result: domain.ResultIgnored}
}

// unmappable escalates an event that cannot be tied to a resource.
//
// Recorded as a warning rather than a failure: nothing went wrong, the event
// simply did not carry enough to act on precisely, and a full reconciliation is
// the correct conservative answer.
func unmappable(reason string) Classification {
	return Classification{
		Request: domain.RefreshFull,
		Result:  domain.ResultWarning,
		Reason:  reason,
	}
}
