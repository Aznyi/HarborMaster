import type { Pagination } from "./inventoryTypes";

/**
 * Notification types: destinations, the rules that route to them, and the
 * record of what was sent.
 *
 * # There is no credential in this file
 *
 * A Slack, Discord, or Teams webhook URL is a bearer token in the shape of a
 * path: anyone who reads one can post into that channel forever. So the URL is
 * a field on the REQUEST type and on nothing else. `NotificationDestination`
 * carries `endpoint`, which is a scheme and host â€” `https://hooks.slack.com` â€”
 * and that is what every list, detail, and delivery record shows.
 *
 * A type with nowhere to put a credential is a stronger guarantee than a
 * component that remembers not to render one.
 *
 * # What the interface must not get wrong
 *
 *  1. **Whether delivery is actually on.** Configuration stays editable when
 *     sending is switched off, which is the right behaviour and the easiest
 *     thing to misread. `enabled` on the status is the answer, and the page
 *     says so at the top rather than leaving an operator to infer it from an
 *     empty history.
 *  2. **What `suppressed` and `dropped` mean.** They are not failures and they
 *     are not successes. `suppressed` is a cooldown deciding not to send;
 *     `dropped` is HarborMaster LOSING a notification because its queue was
 *     full, which is the one outcome that means something went wrong inside
 *     HarborMaster rather than outside it.
 *  3. **That a destination can be failing while everything else is fine.** A
 *     revoked webhook URL breaks notifications silently, which is the worst
 *     possible failure for a subsystem whose whole job is to tell you things.
 */

// ------------------------------------------------------------ vocabularies --

/** Where a destination sends. A closed set; there is no plugin mechanism. */
export type NotificationChannel =
  | "webhook"
  | "slack"
  | "discord"
  | "teams"
  | "email";

export const NOTIFICATION_CHANNEL_ORDER: NotificationChannel[] = [
  "slack",
  "discord",
  "teams",
  "webhook",
  "email",
];

export const NOTIFICATION_CHANNEL_LABELS: Record<NotificationChannel, string> = {
  webhook: "Generic webhook",
  slack: "Slack",
  discord: "Discord",
  teams: "Microsoft Teams",
  email: "Email",
};

export const NOTIFICATION_CHANNEL_DESCRIPTIONS: Record<
  NotificationChannel,
  string
> = {
  webhook:
    "A JSON POST to a URL you control. The body is HarborMaster's own event shape.",
  slack: "An incoming webhook URL from a Slack app.",
  discord: "A channel webhook URL from Discord's channel settings.",
  teams: "An incoming webhook URL from a Microsoft Teams connector.",
  email:
    "Plain-text email through the SMTP relay this deployment was configured with.",
};

/** How much a notification matters. A rule delivers at or above its threshold. */
export type NotificationSeverity = "info" | "warning" | "critical";

export const NOTIFICATION_SEVERITY_ORDER: NotificationSeverity[] = [
  "info",
  "warning",
  "critical",
];

export const NOTIFICATION_SEVERITY_LABELS: Record<
  NotificationSeverity,
  string
> = {
  info: "Everything",
  warning: "Warnings and worse",
  critical: "Only critical",
};

/** What happened. A closed vocabulary written by HarborMaster. */
export type NotificationEvent =
  | "update.discovered"
  | "update.approvalRequired"
  | "acquisition.succeeded"
  | "acquisition.failed"
  | "execution.succeeded"
  | "execution.failed"
  | "rollback.started"
  | "rollback.succeeded"
  | "rollback.failed"
  | "automation.paused"
  | "automation.error"
  | "dependency.rebindFailed"
  | "drift.detected"
  | "policy.violation"
  | "registry.unavailable"
  | "backup.failed"
  | "integrity.failed"
  | "test";

export const NOTIFICATION_EVENT_LABELS: Record<string, string> = {
  "update.discovered": "An update is available",
  "update.approvalRequired": "An update needs approval",
  "acquisition.succeeded": "An image was pulled",
  "acquisition.failed": "An image could not be pulled",
  "execution.succeeded": "A container was updated",
  "execution.failed": "A container could not be updated",
  "rollback.started": "A rollback started",
  "rollback.succeeded": "A container was rolled back",
  "rollback.failed": "A rollback failed",
  "automation.paused": "Automation paused a container",
  "automation.error": "An automation pass could not complete",
  // Never "could not be updated": a reattachment moves no version. It recreates
  // a container on the digest it is already running so it can attach to the
  // replacement of a container whose namespace it shares.
  "dependency.rebindFailed": "A container could not be reattached to a replaced container",
  "drift.detected": "A container drifted from its snapshot",
  "policy.violation": "A container failed a compliance rule",
  "registry.unavailable": "A registry cannot be reached",
  "backup.failed": "A backup failed",
  "integrity.failed": "The database failed its integrity check",
  test: "Test notification",
};

/**
 * Which events are grouped together in the rule editor.
 *
 * Ordered so the first group is the one most deployments want: the things that
 * went wrong.
 */
export const NOTIFICATION_EVENT_GROUPS: {
  label: string;
  hint: string;
  events: NotificationEvent[];
}[] = [
  {
    label: "Things that went wrong",
    hint: "The set most deployments want. A failed rollback is always critical.",
    events: [
      "execution.failed",
      "rollback.failed",
      "rollback.started",
      "acquisition.failed",
      "automation.paused",
      "automation.error",
      "dependency.rebindFailed",
      "integrity.failed",
      "backup.failed",
    ],
  },
  {
    label: "Updates",
    hint: "Discovery and progress. Noisier on a large estate.",
    events: [
      "update.discovered",
      "update.approvalRequired",
      "acquisition.succeeded",
      "execution.succeeded",
      "rollback.succeeded",
    ],
  },
  {
    label: "Compliance and the platform",
    hint: "Configuration drift, policy violations, and registry health.",
    events: ["drift.detected", "policy.violation", "registry.unavailable"],
  },
];

/** What happened to one delivery. */
export type DeliveryResult =
  | "pending"
  | "retrying"
  | "succeeded"
  | "failed"
  | "suppressed"
  | "dropped";

export const DELIVERY_RESULT_LABELS: Record<DeliveryResult, string> = {
  pending: "Sending",
  retrying: "Retrying",
  succeeded: "Delivered",
  failed: "Not delivered",
  suppressed: "Suppressed",
  dropped: "Lost",
};

/**
 * What each outcome MEANS, in the words an operator needs.
 *
 * `suppressed` and `dropped` are the two nobody guesses correctly, and getting
 * them wrong leads in opposite directions: one is HarborMaster working as
 * configured, the other is HarborMaster losing something.
 */
export const DELIVERY_RESULT_DESCRIPTIONS: Record<DeliveryResult, string> = {
  pending: "HarborMaster is sending this now.",
  retrying:
    "The destination did not accept it. HarborMaster will try again on a backing-off schedule.",
  succeeded: "The destination accepted it.",
  failed:
    "HarborMaster gave up. Either the failure was permanent â€” a revoked webhook URL â€” or every retry was used.",
  suppressed:
    "A rule's cooldown decided not to send a repeat of something you had already been told about. Nothing is wrong.",
  dropped:
    "HarborMaster LOST this notification because its queue was full. Something was happening faster than notifications could be delivered.",
};

/** Which outcomes read as a problem, for badge colouring. */
export const DELIVERY_RESULT_TONE: Record<
  DeliveryResult,
  "ok" | "warn" | "bad" | "muted"
> = {
  pending: "muted",
  retrying: "warn",
  succeeded: "ok",
  failed: "bad",
  suppressed: "muted",
  dropped: "bad",
};

// -------------------------------------------------------------- resources --

/**
 * Somewhere notifications are sent â€” the part that is safe to render.
 *
 * `endpoint` is a scheme and host. There is no field on this type for the
 * webhook URL or the SMTP password, and no endpoint in the API returns one.
 */
export interface NotificationDestination {
  destinationId: string;
  name: string;
  description?: string;
  channel: NotificationChannel;
  enabled: boolean;
  /** A scheme and host, never the full URL. The path IS the credential. */
  endpoint: string;
  titlePrefix?: string;
  emailTo?: string[];
  emailFrom?: string;

  lastResult?: DeliveryResult;
  lastAttemptAt?: string;
  /** HarborMaster's own sentence, never the transport's text. */
  lastError?: string;
  consecutiveFailures: number;

  archived: boolean;
  createdAt: string;
  updatedAt: string;
}

/**
 * The create and edit body.
 *
 * `url` travels one way. It is sent here and returned by nothing; omitting it
 * on an edit keeps the stored credential, which is what lets somebody rename a
 * destination without re-typing a webhook URL they may not have kept.
 */
export interface NotificationDestinationRequest {
  name?: string;
  description?: string;
  /** Required on create, refused on edit. */
  channel?: NotificationChannel;
  enabled?: boolean;
  titlePrefix?: string;
  url?: string;
  emailTo?: string[];
  emailFrom?: string;
}

export interface NotificationDestinationResult {
  destination: NotificationDestination;
  /** Legal but worth seeing. Not refusals. */
  warnings?: string[];
}

/** What routes notifications to which destinations. */
export interface NotificationRule {
  ruleId: string;
  name: string;
  enabled: boolean;
  /** EMPTY MEANS EVERY EVENT â€” the opposite of an update policy's selector. */
  events?: NotificationEvent[];
  minimumSeverity: NotificationSeverity;
  destinations: string[];
  /** Zero disables suppression, so every occurrence is sent. */
  cooldownSeconds: number;
  archived: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface NotificationRuleRequest {
  name?: string;
  enabled?: boolean;
  events?: NotificationEvent[];
  minimumSeverity?: NotificationSeverity;
  destinations?: string[];
  cooldownSeconds?: number;
}

export interface NotificationRuleResult {
  rule: NotificationRule;
  warnings?: string[];
}

/**
 * The record of one notification going to one destination.
 *
 * Carries the payload HarborMaster sent, so "what did it actually say" is
 * answerable. It carries no destination URL, no password, and no response
 * body: the first two are credentials and the third is third-party text.
 */
export interface NotificationDelivery {
  deliveryId: string;
  destinationId: string;
  destinationName: string;
  channel: NotificationChannel;
  ruleId?: string;
  ruleName?: string;

  event: NotificationEvent;
  severity: NotificationSeverity;
  title: string;
  body?: string;
  containerName?: string;

  result: DeliveryResult;
  attempts: number;
  statusCode?: number;
  /** HarborMaster's own sentence about the failure. */
  error?: string;
  dedupKey?: string;

  queuedAt: string;
  completedAt?: string;
  nextAttemptAt?: string;
  durationMs?: number;
}

/** The subsystem's state and the vocabularies a rule editor is built from. */
export interface NotificationStatus {
  /**
   * Whether DELIVERY is on. Configuration and history stay readable either
   * way, which is the thing an operator most easily misreads.
   */
  enabled: boolean;
  destinations: number;
  rules: number;
  /** Destinations whose last attempt did not succeed. */
  failing: number;
  delivered?: number;
  failed?: number;
  pending?: number;
  suppressed?: number;
  dropped?: number;
  lastDeliveryAt?: string;

  channels: NotificationChannel[];
  events: NotificationEvent[];
  severities: NotificationSeverity[];
}

// ---------------------------------------------------------------- queries --

export interface NotificationConfigQuery {
  page?: number;
  pageSize?: number;
  includeArchived?: boolean;
}

export interface DeliveryQuery {
  page?: number;
  pageSize?: number;
  destinationId?: string;
  container?: string;
  result?: DeliveryResult[];
  event?: NotificationEvent[];
  /** Only the dead letter â€” what HarborMaster tried to say and could not. */
  failed?: boolean;
}

export interface NotificationDestinationListResponse {
  items: NotificationDestination[];
  pagination: Pagination;
}

export interface NotificationRuleListResponse {
  items: NotificationRule[];
  pagination: Pagination;
}

export interface NotificationDeliveryListResponse {
  items: NotificationDelivery[];
  pagination: Pagination;
}

export interface TestNotificationResponse {
  status: "queued";
  detail: string;
}

