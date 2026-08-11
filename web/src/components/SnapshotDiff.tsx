import type { DiffEntry, DiffGroup, SnapshotDiff } from "../api/snapshotTypes";
import { ChangeBadge } from "./SnapshotBadges";

/**
 * Renders a configuration comparison.
 *
 * Two properties this component must preserve:
 *
 *  - A sensitive entry shows THAT it changed and never what to. The payload
 *    carries no value and no digest for one, so there is nothing to render
 *    even by mistake.
 *  - Truncation is always visible. A truncated diff that looked complete would
 *    read as "these configurations are identical", which is the worst possible
 *    thing to tell someone preparing a restore.
 */
export function SnapshotDiffView({ diff }: { diff: SnapshotDiff }) {
  const changedGroups = diff.groups.filter(
    (group) => group.entries.length > 0 || group.truncated,
  );

  return (
    <div className="flex flex-col gap-4">
      <DiffSummary diff={diff} />

      {diff.truncated ? (
        <p
          role="alert"
          className="rounded-lg border border-warn/40 bg-warn-soft px-4 py-3 text-sm"
        >
          <strong className="font-semibold">This comparison is incomplete.</strong>{" "}
          {diff.truncationReason ||
            "Some entries were not compared or not returned."}{" "}
          Treat the absence of a change below as unknown rather than as no change.
        </p>
      ) : null}

      {changedGroups.length === 0 ? (
        <p className="rounded-lg border border-border-subtle bg-surface-sunken px-4 py-6 text-center text-sm text-content-muted">
          {diff.identical
            ? "No differences. The configurations are identical."
            : "No differences in the selected groups."}
        </p>
      ) : (
        changedGroups.map((group) => <DiffGroupTable key={group.name} group={group} />)
      )}
    </div>
  );
}

function DiffSummary({ diff }: { diff: SnapshotDiff }) {
  const stats: { label: string; value: number }[] = [
    { label: "Added", value: diff.addedCount ?? 0 },
    { label: "Removed", value: diff.removedCount ?? 0 },
    { label: "Modified", value: diff.modifiedCount ?? 0 },
    { label: "Unchanged", value: diff.unchangedCount ?? 0 },
  ];

  return (
    <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      {stats.map((stat) => (
        <div
          key={stat.label}
          className="rounded-lg border border-border-subtle bg-surface-raised px-4 py-3"
        >
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            {stat.label}
          </dt>
          <dd className="mt-1 text-lg font-semibold tabular-nums">{stat.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function DiffGroupTable({ group }: { group: DiffGroup }) {
  return (
    <section className="overflow-hidden rounded-xl border border-border-subtle bg-surface-raised">
      <header className="flex flex-wrap items-center justify-between gap-2 border-b border-border-subtle px-4 py-3">
        <h3 className="text-sm font-semibold capitalize">{group.name}</h3>
        <p className="text-xs text-content-muted">
          {group.returned ?? group.entries.length} of {group.total ?? group.entries.length}{" "}
          shown
          {group.truncated ? " — truncated" : ""}
        </p>
      </header>

      {group.truncated ? (
        <p className="border-b border-warn/40 bg-warn-soft px-4 py-2 text-xs">
          This group stopped short of comparing or returning everything.
        </p>
      ) : null}

      <div className="overflow-x-auto" tabIndex={0}>
        <table className="w-full text-left text-sm">
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th scope="col" className="px-4 py-2">Key</th>
              <th scope="col" className="px-4 py-2">Change</th>
              <th scope="col" className="px-4 py-2">Before</th>
              <th scope="col" className="px-4 py-2">After</th>
            </tr>
          </thead>
          <tbody>
            {group.entries.map((entry) => (
              <DiffRow key={entry.key} entry={entry} />
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function DiffRow({ entry }: { entry: DiffEntry }) {
  return (
    <tr className="border-b border-border-subtle last:border-0 align-top">
      <td className="px-4 py-2 font-mono text-xs">{entry.key}</td>
      <td className="px-4 py-2">
        <ChangeBadge kind={entry.kind} />
      </td>
      <td className="px-4 py-2">
        <DiffValue value={entry.old} sensitive={entry.sensitive} />
      </td>
      <td className="px-4 py-2">
        <DiffValue value={entry.new} sensitive={entry.sensitive} />
      </td>
    </tr>
  );
}

/**
 * One side of a comparison.
 *
 * A sensitive entry never has a value to show -- the API does not send one --
 * so this renders an explanation rather than an empty cell, which would read as
 * "nothing was set".
 */
function DiffValue({ value, sensitive }: { value?: string; sensitive?: boolean }) {
  if (sensitive) {
    return (
      <span className="text-xs text-content-muted">
        <span aria-hidden="true" className="font-mono">
          ********
        </span>{" "}
        not stored
      </span>
    );
  }
  if (value === undefined || value === "") {
    return <span className="text-xs text-content-muted">—</span>;
  }
  return <span className="break-all font-mono text-xs">{value}</span>;
}
