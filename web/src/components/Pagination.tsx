import type { Pagination as PaginationMeta } from "../api/inventoryTypes";

/**
 * Server-side pagination controls.
 *
 * The component renders what the server reported and asks for a page number;
 * it never slices a local array. Rows are bounded by the API, so the browser
 * never holds the whole inventory.
 */
export function Pagination({
  pagination,
  onPageChange,
  busy = false,
}: {
  pagination: PaginationMeta;
  onPageChange: (page: number) => void;
  busy?: boolean;
}) {
  const { page, pageSize, totalItems, totalPages, hasNext, hasPrevious } = pagination;

  if (totalItems === 0) return null;

  const firstRow = (page - 1) * pageSize + 1;
  const lastRow = Math.min(page * pageSize, totalItems);

  return (
    <nav
      aria-label="Pagination"
      className="flex flex-wrap items-center justify-between gap-3 border-t border-border-subtle px-4 py-3 text-sm"
    >
      <p className="text-content-muted" aria-live="polite">
        Showing <strong className="text-content">{firstRow}</strong>&ndash;
        <strong className="text-content">{lastRow}</strong> of{" "}
        <strong className="text-content">{totalItems}</strong>
      </p>

      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => onPageChange(page - 1)}
          disabled={!hasPrevious || busy}
          className="rounded-lg border border-border-subtle px-3 py-1.5 font-medium transition-colors hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-40"
        >
          Previous
        </button>
        <span className="text-content-muted">
          Page {page} of {Math.max(totalPages, 1)}
        </span>
        <button
          type="button"
          onClick={() => onPageChange(page + 1)}
          disabled={!hasNext || busy}
          className="rounded-lg border border-border-subtle px-3 py-1.5 font-medium transition-colors hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-40"
        >
          Next
        </button>
      </div>
    </nav>
  );
}
