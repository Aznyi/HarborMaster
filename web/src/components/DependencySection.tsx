import type { ReactNode } from "react";
import { Link } from "react-router";

import type {
  ContainerDependencies,
  DependencyProblem,
  WorkloadDependency,
} from "../api/dependencyTypes";
import {
  dependencyKindLabel,
  dependencyOriginLabel,
  discoveryRefusalLabel,
  memberStateLabel,
  namespaceExplanation,
} from "../api/dependencyTypes";
import { DetailSection } from "./DetailSection";

/**
 * A container's dependencies, in both directions.
 *
 * # Written for somebody who has never heard of a namespace
 *
 * The two questions an operator actually has are "what has to happen before this
 * container updates" and "what breaks if I update it". So the section is two
 * lists with those headings, and every relationship says in a sentence why it
 * exists.
 *
 * # "None" and "unknown" are different answers
 *
 * A container HarborMaster has not inspected is not a container with no
 * dependencies. Rendering the second when the first is true would be the UI
 * telling an operator it is safe to proceed on the strength of a question
 * nobody asked — so an unresolved namespace share renders as UNAVAILABLE, in
 * its own block, above the lists.
 */
export function DependencySection({
  dependencies,
  unavailable,
  status,
}: {
  dependencies: ContainerDependencies | undefined;
  /** True when the dependency service could not answer at all. */
  unavailable?: boolean;
  /**
   * What is happening to these relationships right now, if anything.
   *
   * Supplied by the caller rather than derived here, because it comes from the
   * container's attention block -- the same source the headline badge reads, so
   * the two cannot disagree about whether a reattachment failed.
   *
   * Rendered ABOVE the lists: current activity changes what the topology below
   * means. Absent when there is nothing happening, which is most of the time.
   */
  status?: ReactNode;
}) {
  if (unavailable) {
    return (
      <DetailSection
        title="Dependencies"
        description="What must be stable before this container is updated."
      >
        <p className="text-sm text-content-muted" role="status">
          Dependency information unavailable. HarborMaster could not establish
          what this container depends on, so it will not reorder or update it
          until it can.
        </p>
      </DetailSection>
    );
  }

  if (!dependencies) {
    return (
      <DetailSection
        title="Dependencies"
        description="What must be stable before this container is updated."
      >
        <p className="text-sm text-content-muted">Loading dependencies…</p>
      </DetailSection>
    );
  }

  const problems = dependencies.problems ?? [];
  const rebinds = dependencies.outstandingRebinds ?? [];
  const nothing =
    dependencies.dependsOn.length === 0 &&
    dependencies.dependedOnBy.length === 0 &&
    problems.length === 0;

  return (
    <DetailSection
      title="Dependencies"
      description="What must be stable before this container is updated, and what depends on it."
    >
      <div className="space-y-5">
        {status}
        {problems.length > 0 ? <ProblemList problems={problems} /> : null}

        {rebinds.length > 0 ? (
          <section aria-label="Reattachment required">
            <h4 className="text-sm font-semibold">Reattachment required</h4>
            <ul className="mt-2 space-y-2">
              {rebinds.map((member) => (
                <li
                  key={`${member.operationId}:${member.dependent}`}
                  className="rounded-lg border border-border-subtle p-3 text-sm"
                >
                  <p className="break-all font-medium">{member.provider}</p>
                  <p className="mt-1 text-content-muted">
                    This container must be recreated on the image it is already
                    running so it can attach to {member.provider}&apos;s
                    replacement. Its image is not being changed.
                  </p>
                  <p className="mt-1 text-xs text-content-muted">
                    {memberStateLabel(member.state)}
                  </p>
                </li>
              ))}
            </ul>
          </section>
        ) : null}

        {nothing ? (
          <p className="text-sm text-content-muted">
            HarborMaster has not detected or been configured with any
            dependencies for this container. It can be updated independently.
          </p>
        ) : null}

        {dependencies.dependsOn.length > 0 ? (
          <RelationshipList
            heading="This container depends on"
            /* The OTHER end of the arrow is what an operator wants to click. */
            nameOf={(edge) => edge.dependency}
            subject={dependencies.container}
            edges={dependencies.dependsOn}
          />
        ) : null}

        {dependencies.dependedOnBy.length > 0 ? (
          <RelationshipList
            heading="Containers depending on this one"
            nameOf={(edge) => edge.dependent}
            subject={dependencies.container}
            edges={dependencies.dependedOnBy}
            dependentSide
          />
        ) : null}
      </div>
    </DetailSection>
  );
}

/**
 * The relationships in one direction.
 *
 * A semantic list, so a screen reader announces how many there are and the
 * direction is carried by the heading rather than by layout.
 */
function RelationshipList({
  heading,
  edges,
  nameOf,
  subject,
  dependentSide = false,
}: {
  heading: string;
  edges: WorkloadDependency[];
  nameOf: (edge: WorkloadDependency) => string;
  subject: string;
  /** True when the listed containers are the DEPENDENTS of the subject. */
  dependentSide?: boolean;
}) {
  return (
    <section aria-label={heading}>
      <h4 className="text-sm font-semibold">{heading}</h4>
      <ul className="mt-2 space-y-2">
        {edges.map((edge) => {
          const name = nameOf(edge);
          // The explanation is always written dependent-first, whichever
          // direction the list is showing.
          const explanation = namespaceExplanation(
            edge.source,
            dependentSide ? name : subject,
            dependentSide ? subject : name,
          );
          return (
            <li
              key={`${edge.dependent}->${edge.dependency}:${edge.source}`}
              className="rounded-lg border border-border-subtle p-3 text-sm"
            >
              <Link
                to={`/containers?search=${encodeURIComponent(name)}`}
                className="break-all font-medium text-accent underline"
              >
                {name}
              </Link>
              <p className="mt-1 flex flex-wrap gap-x-2 gap-y-1 text-xs text-content-muted">
                {/* Kind and origin are separate words, not a colour or an
                    icon: the distinction between what Docker enforces and what
                    an operator asserted has to survive a greyscale screen. */}
                <span>{dependencyKindLabel(edge.source)}</span>
                <span aria-hidden="true">·</span>
                <span>{dependencyOriginLabel(edge.source)}</span>
              </p>
              {explanation ? (
                <p className="mt-2 text-content-muted">{explanation}</p>
              ) : (
                <p className="mt-2 text-content-muted">
                  {dependentSide
                    ? `${name} waits for ${subject} to finish updating.`
                    : `${subject} waits for ${name} to finish updating.`}
                </p>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

/**
 * Namespace shares HarborMaster could not resolve.
 *
 * Rendered ABOVE the relationship lists and marked as an alert, because it
 * changes what the lists below mean: a container with an unresolved share is
 * blocked whatever else is shown.
 */
function ProblemList({ problems }: { problems: DependencyProblem[] }) {
  return (
    // The live region is the SECTION, not the list items.
    //
    // role="status" on an <li> replaces its implicit listitem role, which
    // leaves the <ul> containing a child that is not a list item -- a serious
    // axe violation, and a real one: a screen reader stops announcing the list
    // and its length. Announcing the block as a whole is also the better
    // behaviour, since the items are read together.
    <section aria-label="Dependency information unavailable" role="status">
      <h4 className="text-sm font-semibold text-warning">
        Dependency information unavailable
      </h4>
      <ul className="mt-2 space-y-2">
        {problems.map((problem, index) => (
          <li
            key={`${problem.container}:${problem.refusal}:${index}`}
            className="rounded-lg border border-warning/40 bg-warning-soft p-3 text-sm"
          >
            <p>{discoveryRefusalLabel(problem.refusal)}</p>
            <p className="mt-1 text-xs text-content-muted">
              HarborMaster will not update this container until it can establish
              what it depends on.
            </p>
          </li>
        ))}
      </ul>
    </section>
  );
}
