import { useMemo, useState } from "react";

import { ApiError } from "../api/client";
import type { PolicyDefinition, PolicyRequest } from "../api/policyTypes";
import { RULE_LABELS } from "../api/policyTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { PolicyEditor } from "../components/PolicyEditor";
import { PolicySeverityBadge, PolicyStateBadge } from "../components/PolicyBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  editPolicy,
  savePolicy,
  usePolicies,
  usePolicyRules,
  withdrawPolicy,
} from "../hooks/usePolicies";

const PAGE_SIZE = 25;

/**
 * The policy catalogue.
 *
 * A policy is a rule that HarborMaster CHECKS a container's configuration
 * against. Nothing on this page applies, enforces, or pushes anything to
 * Docker — there is no such control, because there is no such capability.
 *
 * Withdrawing a policy ARCHIVES it. The definition and every violation it found
 * are kept; it simply stops being evaluated. That is what the confirmation says,
 * because "delete" would promise a destruction that does not happen.
 */
export function Policies() {
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState("");
  const [includeArchived, setIncludeArchived] = useState(false);
  const [editing, setEditing] = useState<PolicyDefinition | "new" | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      includeArchived,
      ...(search.trim() ? { search: search.trim() } : {}),
    }),
    [page, search, includeArchived],
  );

  const catalogue = usePolicyRules();
  const policies = usePolicies(query);

  const submit = async (body: PolicyRequest) => {
    if (editing === "new") {
      await savePolicy(body);
    } else if (editing) {
      await editPolicy(editing.policyId, body);
    }
    setEditing(null);
    policies.refresh();
  };

  const withdraw = async (policy: PolicyDefinition) => {
    setActionError(null);
    try {
      await withdrawPolicy(policy.policyId);
      policies.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The policy could not be withdrawn.",
      );
    }
  };

  return (
    <div className="space-y-6">
      <PageIntro
        title="Policies"
        description={
          "Rules that every container's configuration is checked against. " +
          "HarborMaster reports compliance; it cannot change a container to " +
          "achieve it."
        }
      />

      {editing && catalogue.data ? (
        <PolicyEditor
          catalogue={catalogue.data}
          {...(editing === "new" ? {} : { policy: editing })}
          onSubmit={submit}
          onCancel={() => setEditing(null)}
        />
      ) : (
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-xs text-content-muted">
            Search
            <input
              type="search"
              className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
              value={search}
              onChange={(event) => {
                setSearch(event.target.value);
                setPage(1);
              }}
            />
          </label>

          <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
            <input
              type="checkbox"
              className="size-4 rounded border-border-subtle"
              checked={includeArchived}
              onChange={(event) => {
                setIncludeArchived(event.target.checked);
                setPage(1);
              }}
            />
            Include withdrawn
          </label>

          <button
            type="button"
            className="ml-auto rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
            disabled={!catalogue.data}
            onClick={() => setEditing("new")}
          >
            New policy
          </button>
        </div>
      )}

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      <PolicyList
        state={policies}
        onPage={setPage}
        onEdit={setEditing}
        onWithdraw={withdraw}
        editorOpen={editing !== null}
      />
    </div>
  );
}

function PolicyList({
  state,
  onPage,
  onEdit,
  onWithdraw,
  editorOpen,
}: {
  state: ReturnType<typeof usePolicies>;
  onPage: (page: number) => void;
  onEdit: (policy: PolicyDefinition) => void;
  onWithdraw: (policy: PolicyDefinition) => void;
  editorOpen: boolean;
}) {
  if (state.status === "loading") return <LoadingState label="Loading policies" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const data = state.data;
  const items = data?.items ?? [];

  if (items.length === 0) {
    return (
      <EmptyState
        title="No policies defined"
        description={
          "Nothing is being checked yet. An estate with no policies has not " +
          "been found compliant — it has not been asked anything."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((policy) => (
          <PolicyRow
            key={policy.policyId}
            policy={policy}
            onEdit={onEdit}
            onWithdraw={onWithdraw}
            actionsDisabled={editorOpen}
          />
        ))}
      </ul>
      {data?.pagination && (
        <Pagination
          pagination={data.pagination}
          onPageChange={onPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}

function PolicyRow({
  policy,
  onEdit,
  onWithdraw,
  actionsDisabled,
}: {
  policy: PolicyDefinition;
  onEdit: (policy: PolicyDefinition) => void;
  onWithdraw: (policy: PolicyDefinition) => void;
  actionsDisabled: boolean;
}) {
  const [confirming, setConfirming] = useState(false);

  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium text-content">{policy.name}</span>
            <PolicySeverityBadge severity={policy.severity} />
            <PolicyStateBadge policy={policy} />
          </div>

          {policy.description && (
            <p className="max-w-prose text-sm text-content-muted">
              {policy.description}
            </p>
          )}

          <ul className="flex flex-wrap gap-1.5">
            {policy.rules.map((rule) => (
              <li
                key={rule.type}
                className="rounded border border-border-subtle px-1.5 py-0.5 text-xs text-content-muted"
              >
                {RULE_LABELS[rule.type] ?? rule.type}
              </li>
            ))}
          </ul>
        </div>

        {!policy.archived && (
          <div className="flex shrink-0 items-center gap-2">
            <button
              type="button"
              disabled={actionsDisabled}
              onClick={() => onEdit(policy)}
              className="rounded-md border border-border-subtle px-2 py-1 text-xs text-content disabled:opacity-60"
            >
              Edit
            </button>

            {confirming ? (
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => {
                    setConfirming(false);
                    onWithdraw(policy);
                  }}
                  className="rounded-md border border-danger/40 px-2 py-1 text-xs text-danger"
                >
                  Confirm
                </button>
                <button
                  type="button"
                  onClick={() => setConfirming(false)}
                  className="rounded-md px-2 py-1 text-xs text-content-muted"
                >
                  Keep
                </button>
              </div>
            ) : (
              <button
                type="button"
                disabled={actionsDisabled}
                onClick={() => setConfirming(true)}
                className="rounded-md border border-border-subtle px-2 py-1 text-xs text-content-muted hover:text-danger disabled:opacity-60"
                // The wording matters: this archives. The definition and every
                // violation it found are kept.
                title="Stop evaluating this policy. Its definition and the violations it found are kept as history."
              >
                Withdraw
              </button>
            )}
          </div>
        )}
      </div>

      {confirming && (
        <p role="status" className="mt-2 text-xs text-content-muted">
          Withdrawing stops the policy being evaluated and resolves its open
          violations. The definition and the history of what it caught are kept.
        </p>
      )}
    </li>
  );
}
