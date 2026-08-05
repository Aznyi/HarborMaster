import { useState } from "react";
import { Link, useParams } from "react-router";

import type { ImageIntel } from "../api/imageTypes";
import { DetailSection } from "../components/DetailSection";
import {
  CheckStatusBadge,
  DigestComparison,
  RegistryBadge,
  UpdateBadge,
} from "../components/ImageBadges";
import { ImageUpdateTimeline } from "../components/ImageUpdateTimeline";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useImageDetail, useImageHistory } from "../hooks/useImageUpdates";

/**
 * One image, with what the local daemon knows and what the registry reports.
 *
 * The two halves answer different questions and are shown together on purpose:
 * "what is this image" is local, "is there a newer one" is remote, and an
 * operator looking at an image wants both.
 *
 * Read-only, like every other page. There is no pull control and no update
 * button: HarborMaster reports that a newer image exists and has no capability
 * to fetch it.
 */
export function ImageDetailPage() {
  const { id = "" } = useParams();
  const [page, setPage] = useState(1);

  const detail = useImageDetail(id);
  const history = useImageHistory(id, page);

  if (detail.status === "loading") return <LoadingState label="Loading image" />;
  if (detail.status === "disconnected") {
    return <DisconnectedState onRetry={detail.refresh} />;
  }
  if (detail.error) {
    return <ErrorState error={detail.error} onRetry={detail.refresh} />;
  }

  // Tolerate a malformed payload rather than throwing.
  const image = detail.data?.image;
  const intel = detail.data?.intel ?? [];
  if (!image) return <LoadingState label="Loading image" />;

  return (
    <div className="space-y-6">
      <PageIntro
        title={image.repoTags?.[0] ?? image.shortId ?? "Image"}
        description={
          "What the local daemon records about this image, and what its " +
          "registries report. Reporting only — nothing here pulls or removes " +
          "anything."
        }
      />

      <p className="text-sm text-content-muted">
        <Link to="/images" className="underline underline-offset-2 hover:text-content">
          Back to images
        </Link>
      </p>

      <DetailSection title="Local">
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-content-muted">ID</dt>
          <dd className="font-mono break-all text-content">{image.id}</dd>
          <dt className="text-content-muted">Containers</dt>
          <dd className="text-content">{detail.data?.containerCount ?? 0}</dd>
          {image.architecture && (
            <>
              <dt className="text-content-muted">Platform</dt>
              <dd className="text-content">
                {[image.os, image.architecture, image.variant]
                  .filter(Boolean)
                  .join("/")}
              </dd>
            </>
          )}
          {image.repoTags && image.repoTags.length > 0 && (
            <>
              <dt className="text-content-muted">Tags</dt>
              <dd className="font-mono break-all text-content">
                {image.repoTags.join(", ")}
              </dd>
            </>
          )}
        </dl>
      </DetailSection>

      <DetailSection title="Registry">
        {intel.length === 0 ? (
          <EmptyState
            title="No registry information"
            description={
              "None of this image's references has been checked against a " +
              "registry. That is not the same as having no updates — it means " +
              "nothing has been asked yet, or the reference names no public " +
              "registry."
            }
          />
        ) : (
          <ul className="space-y-3">
            {intel.map((entry) => (
              <IntelCard key={entry.id} intel={entry} />
            ))}
          </ul>
        )}
      </DetailSection>

      <DetailSection title="History">
        <HistoryPanel state={history} onPage={setPage} />
      </DetailSection>
    </div>
  );
}

/** One reference's registry intelligence. */
function IntelCard({ intel }: { intel: ImageIntel }) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-center gap-2">
        <UpdateBadge update={intel.updateType} />
        <RegistryBadge registry={intel.registry} />
        <CheckStatusBadge status={intel.checkStatus} />
      </div>

      <p className="mt-2 font-mono text-sm text-content break-all">
        {intel.familiar}
      </p>

      {intel.latestTag && (
        <p className="mt-1 text-sm text-content">
          Newer tag available: <span className="font-mono">{intel.latestTag}</span>
        </p>
      )}
      {intel.updateReason && (
        <p className="mt-1 text-xs text-content-muted">{intel.updateReason}</p>
      )}
      {intel.statusDetail && (
        <p className="mt-1 text-xs text-warn">{intel.statusDetail}</p>
      )}

      <div className="mt-3">
        <DigestComparison intel={intel} />
      </div>

      <dl className="mt-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
        {intel.publishedAt && (
          <>
            <dt className="text-content-muted">Published</dt>
            <dd className="text-content">
              <time dateTime={intel.publishedAt}>
                {new Date(intel.publishedAt).toLocaleString()}
              </time>
            </dd>
          </>
        )}
        {intel.vendor && (
          <>
            <dt className="text-content-muted">Vendor</dt>
            <dd className="text-content">{intel.vendor}</dd>
          </>
        )}
        {intel.source && (
          <>
            <dt className="text-content-muted">Source</dt>
            {/*
              A publisher-supplied URL. Rendered as TEXT rather than as a link:
              it is third-party content, and turning it into something clickable
              would be handing a registry a way to place a link in HarborMaster's
              UI.
            */}
            <dd className="break-all text-content">{intel.source}</dd>
          </>
        )}
        {intel.lastCheckedAt && (
          <>
            <dt className="text-content-muted">Last checked</dt>
            <dd className="text-content">
              <time dateTime={intel.lastCheckedAt}>
                {new Date(intel.lastCheckedAt).toLocaleString()}
              </time>
            </dd>
          </>
        )}
      </dl>
    </li>
  );
}

function HistoryPanel({
  state,
  onPage,
}: {
  state: ReturnType<typeof useImageHistory>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading history" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const events = state.data?.items ?? [];
  if (events.length === 0) {
    return (
      <EmptyState
        title="Nothing has changed yet"
        description={
          "Only actual changes are recorded — a check that found everything " +
          "unchanged writes nothing. An empty history means the image has been " +
          "stable, or has not been checked yet."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ImageUpdateTimeline events={events} />
      {state.data?.pagination && (
        <Pagination
          pagination={state.data.pagination}
          onPageChange={onPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}
