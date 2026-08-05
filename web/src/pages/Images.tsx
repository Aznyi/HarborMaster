import { useState } from "react";
import { Link } from "react-router";

import { useImagePage } from "../hooks/useContainers";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ScrollArea } from "../components/DetailSection";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";

/**
 * Images referenced by the inventoried containers.
 *
 * HarborMaster records image metadata only. It never pulls, builds, tags, or
 * removes an image.
 */
export function Images() {
  const [page, setPage] = useState(1);
  const images = useImagePage(page);

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Images"
        description="Metadata for the images the inventoried containers run. HarborMaster observes these; it does not pull, build, or remove them."
      />
      <p className="text-sm text-content-muted">
        <Link to="/images/updates" className="text-accent hover:underline">
          Check which of these have a newer version published
        </Link>
      </p>
      <ImageTable images={images} onPageChange={setPage} />
    </div>
  );
}

function ImageTable({
  images,
  onPageChange,
}: {
  images: ReturnType<typeof useImagePage>;
  onPageChange: (page: number) => void;
}) {
  if (images.status === "loading") {
    return <LoadingState label="Loading images" />;
  }
  if (images.status === "disconnected") {
    return <DisconnectedState onRetry={images.refresh} />;
  }
  if (images.error) {
    return <ErrorState error={images.error} onRetry={images.refresh} />;
  }
  if (!images.data) {
    return <LoadingState label="Loading images" />;
  }

  if (images.data.items.length === 0) {
    return (
      <EmptyState
        title="No images recorded"
        description="Images appear here once an inventory refresh has resolved the images its containers run."
      />
    );
  }

  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised">
      <ScrollArea>
        <table className="w-full min-w-[52rem] text-left text-sm">
          <caption className="sr-only">Images</caption>
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th scope="col" className="px-4 py-3 font-medium">Image</th>
              <th scope="col" className="px-4 py-3 font-medium">Digests</th>
              <th scope="col" className="px-4 py-3 font-medium">Created</th>
              <th scope="col" className="px-4 py-3 font-medium">Size</th>
              <th scope="col" className="px-4 py-3 font-medium">Platform</th>
              <th scope="col" className="px-4 py-3 font-medium">Containers</th>
            </tr>
          </thead>
          <tbody>
            {images.data.items.map((usage) => (
              <tr
                key={usage.image.id}
                className="border-b border-border-subtle last:border-0 hover:bg-surface-sunken"
              >
                <td className="px-4 py-3">
                  {usage.image.repoTags.length > 0 ? (
                    <ul className="flex flex-col gap-0.5">
                      {usage.image.repoTags.map((tag) => (
                        <li key={tag} className="break-all font-medium">
                          {tag}
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <span className="text-content-muted">&lt;untagged&gt;</span>
                  )}
                  {/*
                    The short id is the link, so the row reaches the detail view
                    without turning every tag into a separate destination for the
                    same image.
                  */}
                  <Link
                    to={`/images/${usage.image.id}`}
                    className="font-mono text-xs text-accent hover:underline"
                  >
                    {usage.image.shortId}
                  </Link>
                </td>
                <td className="px-4 py-3">
                  {usage.image.repoDigests.length > 0 ? (
                    <ul className="flex flex-col gap-0.5 font-mono text-xs text-content-muted">
                      {usage.image.repoDigests.slice(0, 2).map((digest) => (
                        <li key={digest} className="break-all">
                          {digest}
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <span className="text-xs text-content-muted">none</span>
                  )}
                </td>
                <td className="px-4 py-3 text-xs text-content-muted">
                  {formatDate(usage.image.createdAt)}
                </td>
                <td className="px-4 py-3 tabular-nums">{formatBytes(usage.image.size)}</td>
                <td className="px-4 py-3 text-xs">
                  {usage.image.os && usage.image.architecture
                    ? `${usage.image.os}/${usage.image.architecture}`
                    : "—"}
                </td>
                <td className="px-4 py-3 tabular-nums">{usage.containerCount}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </ScrollArea>

      <Pagination
        pagination={images.data.pagination}
        onPageChange={onPageChange}
        busy={images.refreshing}
      />
    </div>
  );
}

function formatDate(iso: string | undefined): string {
  if (!iso) return "—";
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? iso : parsed.toLocaleDateString();
}

function formatBytes(bytes: number): string {
  if (!bytes) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}
