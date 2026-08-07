import { useCallback } from "react";

import {
  archiveNotificationDestination,
  archiveNotificationRule,
  createNotificationDestination,
  createNotificationRule,
  getNotificationStatus,
  listNotificationDeliveries,
  listNotificationDestinations,
  listNotificationRules,
  testNotificationDestination,
  updateNotificationDestination,
  updateNotificationRule,
} from "../api/client";
import type {
  DeliveryQuery,
  NotificationConfigQuery,
  NotificationDeliveryListResponse,
  NotificationDestinationListResponse,
  NotificationDestinationRequest,
  NotificationDestinationResult,
  NotificationRuleListResponse,
  NotificationRuleRequest,
  NotificationRuleResult,
  NotificationStatus,
  TestNotificationResponse,
} from "../api/notificationTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Notification resources.
 *
 * # What can be reached from here
 *
 * Four reads and seven writes. Every write either administers a destination or
 * a rule, or asks HarborMaster to send a test to a destination it already
 * holds. **No hook here takes a URL, an address, or a message** except the two
 * that create and edit a destination — which is where a credential is supposed
 * to enter, and the only direction it travels.
 *
 * # Polling
 *
 * The delivery history polls while anything is in flight and is left alone
 * otherwise. A settled deployment sends nothing for hours at a time, so a page
 * left open costs nothing.
 */

/**
 * How often the delivery history is re-read while a delivery is in flight.
 *
 * A delivery is bounded by the configured timeout, which defaults to fifteen
 * seconds; three is frequent enough to watch one settle and slow enough that a
 * page left open is not a load source.
 */
const ACTIVE_POLL_MS = 3000;

/** The subsystem's state, counts, and the vocabularies the editors use. */
export function useNotificationStatus(
  options: { poll?: boolean } = {},
): ResourceState<NotificationStatus> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getNotificationStatus({ signal }),
    [],
  );

  return useApiResource<NotificationStatus>(fetcher, {
    key: "notification-status",
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/** Where notifications are sent. Public records only; no credential. */
export function useNotificationDestinations(
  query: NotificationConfigQuery = {},
): ResourceState<NotificationDestinationListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listNotificationDestinations(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<NotificationDestinationListResponse>(fetcher, {
    key: `notification-destinations-${JSON.stringify(query)}`,
  });
}

/** What routes notifications to which destinations. */
export function useNotificationRules(
  query: NotificationConfigQuery = {},
): ResourceState<NotificationRuleListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listNotificationRules(query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<NotificationRuleListResponse>(fetcher, {
    key: `notification-rules-${JSON.stringify(query)}`,
  });
}

/** What was sent, to where, and whether it arrived. */
export function useNotificationDeliveries(
  query: DeliveryQuery = {},
  options: { poll?: boolean } = {},
): ResourceState<NotificationDeliveryListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listNotificationDeliveries(query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<NotificationDeliveryListResponse>(fetcher, {
    key: `notification-deliveries-${JSON.stringify(query)}`,
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

// ----------------------------------------------------------------- writes --

/**
 * Creates a destination.
 *
 * The one write in the UI that sends a CREDENTIAL. The response carries the
 * destination's public record and no URL, so a caller that renders the result
 * cannot accidentally render the secret it just sent.
 */
export function useCreateNotificationDestination(): (
  body: NotificationDestinationRequest,
) => Promise<NotificationDestinationResult> {
  return useCallback(
    (body: NotificationDestinationRequest) =>
      createNotificationDestination(body),
    [],
  );
}

/**
 * Edits a destination.
 *
 * **Omitting `url` keeps the stored credential**, which is what lets somebody
 * rename a destination without re-typing a webhook URL they may not have kept.
 */
export function useUpdateNotificationDestination(): (
  destinationId: string,
  body: NotificationDestinationRequest,
) => Promise<NotificationDestinationResult> {
  return useCallback(
    (destinationId: string, body: NotificationDestinationRequest) =>
      updateNotificationDestination(destinationId, body),
    [],
  );
}

/** Withdraws a destination and destroys its stored credential. */
export function useArchiveNotificationDestination(): (
  destinationId: string,
) => Promise<void> {
  return useCallback(
    (destinationId: string) => archiveNotificationDestination(destinationId),
    [],
  );
}

/**
 * Sends a test notification.
 *
 * Takes an identifier and nothing else. Resolves once the test is QUEUED, not
 * once it is delivered: the caller refreshes the history rather than expecting
 * the outcome inline, which is the same shape every other asynchronous action
 * in HarborMaster has.
 */
export function useTestNotificationDestination(): (
  destinationId: string,
) => Promise<TestNotificationResponse> {
  return useCallback(
    (destinationId: string) => testNotificationDestination(destinationId),
    [],
  );
}

/** Creates a routing rule. */
export function useCreateNotificationRule(): (
  body: NotificationRuleRequest,
) => Promise<NotificationRuleResult> {
  return useCallback(
    (body: NotificationRuleRequest) => createNotificationRule(body),
    [],
  );
}

/** Edits a routing rule. */
export function useUpdateNotificationRule(): (
  ruleId: string,
  body: NotificationRuleRequest,
) => Promise<NotificationRuleResult> {
  return useCallback(
    (ruleId: string, body: NotificationRuleRequest) =>
      updateNotificationRule(ruleId, body),
    [],
  );
}

/** Withdraws a routing rule. */
export function useArchiveNotificationRule(): (
  ruleId: string,
) => Promise<void> {
  return useCallback((ruleId: string) => archiveNotificationRule(ruleId), []);
}
