import { useId, useMemo, useState } from "react";

import type { ContainerListRow } from "../api/inventoryTypes";
import { formatImageReference } from "../api/presentation";
import { useContainerPage } from "../hooks/useContainers";

/**
 * Choosing containers from the inventory, by name.
 *
 * # Why this replaced a comma-separated text field
 *
 * The field it replaces asked an operator to type container names from memory,
 * with no feedback until the next scheduler pass. Every failure mode of that
 * was silent: a typo governed nothing, a renamed container stopped being
 * governed, and neither showed up as anything other than an update that did not
 * happen.
 *
 * # Names, not ids
 *
 * The selection is a list of NAMES, because that is what a policy selector
 * stores. An id changes on every recreation, and a policy pinned to one would
 * stop governing the container the moment it acted — so this control cannot
 * offer ids even though it has them.
 *
 * # A name that is not in the inventory is kept
 *
 * An operator editing a policy whose container is currently stopped, absent, or
 * simply not on this page must not have that name silently dropped. Selected
 * names that no inventory row matches are shown as chips of their own, marked,
 * and are submitted unchanged. Discarding them would be the form quietly
 * narrowing a policy.
 */
export function ContainerPicker({
  selected,
  onChange,
  describedBy,
}: {
  selected: string[];
  onChange: (names: string[]) => void;
  describedBy?: string;
}) {
  const [search, setSearch] = useState("");
  const searchId = useId();
  const listId = useId();

  // The inventory, filtered server-side. Preserved containers are excluded by
  // default by the query, which is the same set the broad scope would consider
  // — so the two ways of choosing containers agree about what a workload is.
  const page = useContainerPage(
    useMemo(
      () => ({ page: 1, pageSize: 50, search: search.trim() || undefined }),
      [search],
    ),
  );

  const rows = page.data?.items ?? [];
  const selectedSet = useMemo(() => new Set(selected), [selected]);

  const toggle = (name: string) => {
    if (selectedSet.has(name)) {
      onChange(selected.filter((entry) => entry !== name));
    } else {
      onChange([...selected, name]);
    }
  };

  // Selected names the current page does not contain. Shown so a selection is
  // never invisible, whatever the search box says.
  const unlisted = selected.filter(
    (name) => !rows.some((row) => row.name === name),
  );

  return (
    // min-w-0 so a long selected name cannot widen whatever contains the
    // picker. The chip truncates, and truncation needs white-space: nowrap,
    // which makes the chip's min-content width the full name.
    <div className="min-w-0 space-y-2">
      <label htmlFor={searchId} className="block text-xs uppercase tracking-wide text-content-muted">
        Search the inventory
      </label>
      <input
        id={searchId}
        type="search"
        className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
        value={search}
        onChange={(event) => setSearch(event.target.value)}
        placeholder="Filter by name or image"
        /*
         * Named by the section it belongs to as WELL as by its own label.
         *
         * A form with two pickers on it -- the dependency editor has exactly
         * that, one for each end of a relationship -- otherwise gives both
         * search boxes the identical accessible name "Search the inventory",
         * and somebody navigating by control name cannot tell which end they
         * are filling in. Combining the two ids yields "Dependent container
         * Search the inventory", which is unambiguous and needs no change from
         * the single-picker callers: they pass no describedBy and keep the
         * label they had.
         */
        aria-labelledby={describedBy ? `${describedBy} ${searchId}` : undefined}
        aria-describedby={describedBy}
        aria-controls={listId}
      />

      {selected.length > 0 ? (
        <ul className="flex flex-wrap gap-1.5" aria-label="Selected containers">
          {selected.map((name) => (
            /*
             * min-w-0 and max-w-full on the ITEM, not only on the button.
             *
             * A flex item sizes to its content by default, so `max-w-full` on
             * the button resolved against a list item that had already grown
             * past the form -- and the truncate never fired. A long container
             * name pushed the whole page 353px wide at 390px, which real
             * Chromium found and jsdom could not: jsdom has no layout.
             */
            <li key={name} className="min-w-0 max-w-full">
              <button
                type="button"
                className="flex min-h-8 max-w-full items-center gap-1.5 rounded-full border border-accent/40 bg-accent-soft px-2.5 py-1 text-xs text-accent"
                onClick={() => toggle(name)}
              >
                <span className="min-w-0 truncate">{name}</span>
                {unlisted.includes(name) ? (
                  <span className="text-content-muted" title="Not in the current inventory listing">
                    (not listed)
                  </span>
                ) : null}
                <span aria-hidden="true">×</span>
                <span className="sr-only">Remove {name}</span>
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-content-muted">
          No containers chosen yet. This policy governs nothing until at least
          one is.
        </p>
      )}

      <div
        id={listId}
        className="max-h-56 overflow-y-auto rounded-lg border border-border-subtle"
      >
        {page.status === "loading" ? (
          <p className="px-3 py-2 text-sm text-content-muted">Loading containers…</p>
        ) : null}
        {page.error ? (
          <p role="alert" className="px-3 py-2 text-sm text-danger">
            The inventory could not be read. You can still name containers under
            Advanced selection.
          </p>
        ) : null}
        {page.status !== "loading" && !page.error && rows.length === 0 ? (
          <p className="px-3 py-2 text-sm text-content-muted">
            No containers match that search.
          </p>
        ) : null}

        <ul>
          {rows.map((row) => (
            <ContainerOption
              key={row.id}
              row={row}
              checked={selectedSet.has(row.name)}
              onToggle={() => toggle(row.name)}
            />
          ))}
        </ul>
      </div>
    </div>
  );
}

function ContainerOption({
  row,
  checked,
  onToggle,
}: {
  row: ContainerListRow;
  checked: boolean;
  onToggle: () => void;
}) {
  const image = formatImageReference(row.image);

  return (
    <li className="border-b border-border-subtle last:border-b-0">
      <label className="flex min-h-11 cursor-pointer items-center gap-2 px-3 py-2 text-sm">
        <input
          type="checkbox"
          className="h-5 w-5 shrink-0 rounded border-border-subtle"
          checked={checked}
          onChange={onToggle}
        />
        <span className="min-w-0 flex-1">
          {/* The NAME wraps rather than truncates: a long name that is cut off
              is a name an operator cannot verify they picked correctly, and the
              name is what this control actually selects. */}
          <span className="block break-all font-medium">{row.name}</span>
          {/* The image is context for telling two similarly named containers
              apart, not the value being chosen, so it takes the compact form.
              Only the digest is shortened and the whole reference stays on the
              title. */}
          <span
            className="block break-all text-xs text-content-muted"
            title={image.abbreviated ? image.full : undefined}
          >
            {image.display}
          </span>
        </span>
      </label>
    </li>
  );
}
