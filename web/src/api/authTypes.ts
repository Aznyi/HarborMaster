/**
 * Types for identity, authorization, and the security audit log.
 *
 * Hand-maintained against `api/openapi.yaml`; that document is the contract.
 *
 * # What is deliberately absent
 *
 * There is no session-token type anywhere in this file, and no field that would
 * carry one. The session token lives in an HttpOnly cookie the browser sends
 * and JavaScript cannot read. The only secret this app ever holds is the CSRF
 * token, which is kept in a module variable in `client.ts` -- never in
 * `localStorage`, `sessionStorage`, a URL, or a React state tree that a devtools
 * extension can walk.
 */

import type { Pagination } from "./inventoryTypes";

/** The three built-in roles. A role outside this set holds nothing. */
export type Role = "viewer" | "operator" | "administrator";

/**
 * A capability a role may hold.
 *
 * A string union rather than an enum so the values are exactly the server's,
 * and a typo is a compile error rather than a silently-false permission check.
 */
export type Permission =
  | "inventory:read"
  | "event:read"
  | "snapshot:read"
  | "drift:read"
  | "policy:read"
  | "plan:read"
  | "acquisition:read"
  | "execution:read"
  | "rollback:read"
  | "automation:read"
  | "notification:read"
  | "inventory:refresh"
  | "snapshot:create"
  | "drift:annotate"
  | "policy:evaluate"
  | "policy:annotate"
  | "image:refresh"
  | "plan:generate"
  | "plan:approve"
  | "acquisition:create"
  | "acquisition:cancel"
  | "execution:create"
  | "execution:cancel"
  | "rollback:create"
  | "rollback:cancel"
  | "automation:run"
  | "automation:approve"
  | "automation:pause"
  | "dependency:read"
  | "dependency:manage"
  | "automation:manage"
  | "notification:manage"
  | "policy:manage"
  | "user:manage"
  | "audit:read";

/** Whether an account may sign in. */
export type UserStatus = "active" | "disabled";

/**
 * An account as the API renders it.
 *
 * No credential, no salt, no hash, no session token: the server's projection
 * carries none of them, and this type could not hold one if it tried.
 */
export interface PublicUser {
  userId: string;
  username: string;
  role: Role;
  status: UserStatus;
  /** Everything the role holds, so the UI need not reimplement the matrix. */
  permissions: Permission[];
  /** The account must choose a new password before doing anything else. */
  mustChangePassword: boolean;
  createdAt: string;
  lastLoginAt?: string;
}

/** What the SPA needs to operate after signing in. */
export interface SessionResponse {
  user: PublicUser;
  /**
   * The CSRF token for this session.
   *
   * It belongs in a body rather than a cookie: a script has to send it back in
   * a header, and it is useless to an attacker without the session cookie that
   * a script cannot read.
   */
  csrfToken: string;
  /** When the session ends regardless of use. */
  expiresAt?: string;
}

/** Whether this installation has an administrator yet. */
export interface BootstrapStatus {
  completed: boolean;
  /** Always true for the web flow: claiming needs the startup token. */
  tokenRequired: boolean;
}

export interface LoginRequest {
  username: string;
  password: string;
}

export interface BootstrapRequest {
  /** The one-time value printed in the server log at startup. */
  token: string;
  username: string;
  password: string;
}

export interface ChangePasswordRequest {
  currentPassword: string;
  newPassword: string;
}

/** A live session on the caller's own account. */
export interface SessionSummary {
  sessionId: string;
  createdAt: string;
  lastSeenAt: string;
  idleExpiresAt: string;
  absoluteExpiresAt: string;
  /** A bounded, sanitised note -- never rendered as markup. */
  userAgent?: string;
  clientAddr?: string;
  /** True for the session making the request. */
  current?: boolean;
}

export interface SessionListResponse {
  items: SessionSummary[];
}

/** One role and what it can do, for the role picker. */
export interface RoleDescription {
  role: Role;
  description: string;
  permissions: Permission[];
}

export interface RoleCatalogue {
  items: RoleDescription[];
}

export interface CreateUserRequest {
  username: string;
  role: Role;
  /** Optional. Omitting it makes the server generate one and return it once. */
  password?: string;
}

/**
 * A newly created account.
 *
 * `temporaryPassword` is present exactly once, in the response to the request
 * that created the account. It is never stored and never retrievable again, so
 * the UI must show it before navigating away.
 */
export interface CreatedUser {
  user: PublicUser;
  temporaryPassword?: string;
}

export interface UpdateUserRequest {
  role?: Role;
  status?: UserStatus;
}

export interface UserListResponse {
  items: PublicUser[];
  pagination?: Pagination;
}

/** Whether an audited action succeeded, failed, or was refused. */
export type AuditOutcome = "succeeded" | "failed" | "denied";

/**
 * One row of the security audit log.
 *
 * Every field is either a closed vocabulary or a bounded, sanitised string the
 * server wrote. There is no request body, no header, and no environment value:
 * see the schema comment in `0011_auth.sql` for why.
 */
export interface AuditEvent {
  eventId: string;
  action: string;
  outcome: AuditOutcome;
  actorUserId?: string;
  actorUsername?: string;
  actorRole?: Role;
  actorSessionId?: string;
  targetType?: string;
  targetId?: string;
  targetName?: string;
  requestId?: string;
  clientAddr?: string;
  reason?: string;
  occurredAt: string;
}

export interface AuditQuery {
  page?: number;
  pageSize?: number;
  action?: string[];
  outcome?: AuditOutcome[];
  actorUserId?: string;
  targetType?: string;
  targetId?: string;
  /** Restrict to authentication, authorization, and account administration. */
  securityOnly?: boolean;
  since?: string;
}

export interface AuditListResponse {
  items: AuditEvent[];
  pagination?: Pagination;
}

/**
 * Counters for the audit page's header.
 *
 * `failedLogins` and `deniedActions` are the two an administrator opens the
 * page for: one is somebody trying to get in, the other is somebody already in
 * trying to reach further than their role allows.
 */
export interface AuditSummary {
  /** Every record retained, not only those in the window. */
  total: number;
  failedLogins: number;
  deniedActions: number;
  /** Host-changing actions: an image acquired, or a container replaced. */
  privilegedActions: number;
  /** How far back the windowed counters look. */
  windowHours: number;
  byAction?: Record<string, number>;
  byOutcome?: Record<AuditOutcome, number>;
  lastEventAt?: string;
}
