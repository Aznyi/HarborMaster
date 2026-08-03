import type { ReactNode } from "react";

/** A titled section of a detail view. */
export function DetailSection({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <section
      aria-label={title}
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h3 className="text-base font-semibold">{title}</h3>
      {description ? (
        <p className="mt-1 max-w-prose text-sm text-content-muted">{description}</p>
      ) : null}
      <div className="mt-4">{children}</div>
    </section>
  );
}

/** A definition list of label/value pairs. */
export function FieldList({ children }: { children: ReactNode }) {
  return <dl className="grid gap-3 text-sm sm:grid-cols-2">{children}</dl>;
}

/**
 * One field. Renders nothing when the value is absent, so a detail view shows
 * only what is actually configured rather than a wall of empty rows.
 */
export function Field({
  label,
  value,
  mono = false,
  span = false,
}: {
  label: string;
  value: ReactNode;
  mono?: boolean;
  span?: boolean;
}) {
  if (value === undefined || value === null || value === "") return null;

  return (
    <div className={span ? "sm:col-span-2" : undefined}>
      <dt className="text-content-muted">{label}</dt>
      <dd className={mono ? "mt-0.5 break-all font-mono text-xs" : "mt-0.5 break-words"}>
        {value}
      </dd>
    </div>
  );
}

/** A boolean field, always rendered so "false" is visible rather than absent. */
export function BoolField({ label, value }: { label: string; value: boolean }) {
  return (
    <div>
      <dt className="text-content-muted">{label}</dt>
      <dd className="mt-0.5">{value ? "yes" : "no"}</dd>
    </div>
  );
}

/**
 * A scrollable, selectable block of preformatted text.
 *
 * No clipboard API is used. The page's Content-Security-Policy is deliberately
 * strict and clipboard access needs a permission prompt, so the block is made
 * easy to select by hand instead: it is focusable, wraps long content, and
 * scrolls inside its own box rather than stretching the page.
 */
export function CodeBlock({ content, label }: { content: string; label: string }) {
  return (
    <div>
      <p className="mb-2 text-xs text-content-muted">
        Select the text below to copy it.
      </p>
      <pre
        tabIndex={0}
        aria-label={label}
        className="max-h-[32rem] overflow-auto rounded-lg border border-border-subtle bg-surface-sunken p-4 font-mono text-xs leading-relaxed select-all"
      >
        {content}
      </pre>
    </div>
  );
}

/** A horizontally scrollable wrapper, so wide tables never scroll the page. */
export function ScrollArea({ children }: { children: ReactNode }) {
  return <div className="overflow-x-auto">{children}</div>;
}
