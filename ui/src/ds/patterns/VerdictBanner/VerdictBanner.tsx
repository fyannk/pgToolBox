import { Link } from "@tanstack/react-router";
import { useId } from "react";
import type { Contributor, Verdict } from "../../../api/generated/model";
import { type ClusterParams, resolveClusterLink } from "../../../app/clusterLink";
import { m } from "../../../paraglide/messages.js";
import { cn } from "../../cn";
import { TINTED_HEALTH } from "../../health";
import { followRowLink } from "../../rowLink";
import { Provenance } from "../Provenance/Provenance";
import { StatusChip } from "../StatusChip/StatusChip";

function ContributorRow({ c, params }: { c: Contributor; params?: ClusterParams }) {
  const target = c.link && params ? resolveClusterLink(c.link, params) : null;
  // A line that is not fine is tinted whole, its label in the state's colour.
  const tinted = TINTED_HEALTH.has(c.health);
  return (
    // biome-ignore lint/a11y/noNoninteractiveElementInteractions: a pointer shortcut to the line's own link, which stays the keyboard target.
    // biome-ignore lint/a11y/useKeyWithClickEvents: the same; the keyboard follows the link itself.
    <li
      data-row-health={tinted ? c.health : undefined}
      // A line with a link is followed from anywhere on it, like a table row.
      data-row-link={target ? "" : undefined}
      onClick={target ? followRowLink : undefined}
      onAuxClick={target ? followRowLink : undefined}
      className={cn(
        "-mx-3 grid grid-cols-[minmax(7rem,auto)_1fr] items-start gap-x-3 gap-y-0.5 border-line border-t px-3 py-2 first:border-t-0 sm:grid-cols-[9rem_12rem_1fr]",
        tinted && "health-row",
      )}
    >
      <StatusChip health={c.health} stale={c.provenance.stale} size="sm" className="justify-self-start" />
      <span className="row-label font-medium">
        {target ? (
          <Link {...target} className="text-accent underline-offset-2 hover:underline">
            {c.label}
          </Link>
        ) : (
          c.label
        )}
      </span>
      <span className="col-span-2 sm:col-span-1">
        <span>{c.detail ?? m.not_reported()}</span> <Provenance provenance={c.provenance} />
      </span>
    </li>
  );
}

/**
 * VerdictBanner — the roll-up headline and every contributor it
 * was computed from, worst first, so the reader sees what was looked at.
 * The worst state is Go's; the banner only draws it.
 */
export function VerdictBanner({ verdict, title, params }: { verdict: Verdict; title: string; params?: ClusterParams }) {
  const id = useId();
  const contributors = verdict.contributors ?? [];
  return (
    <section
      aria-labelledby={id}
      className="health rounded-panel p-4"
      data-health={verdict.health}
      data-stale={verdict.stale ? "" : undefined}
    >
      <div className="flex flex-wrap items-center gap-3">
        <h2 id={id} className="font-semibold text-heading">
          {title}
        </h2>
        <StatusChip health={verdict.health} stale={verdict.stale} size="lg" />
      </div>
      {verdict.stale && <p className="mt-1 text-label">{m.verdict_stale_note()}</p>}
      {contributors.length === 0 ? (
        <p className="mt-2 text-label">{m.verdict_no_contributors()}</p>
      ) : (
        <ul
          className="mt-3 overflow-hidden rounded-panel bg-surface px-3 text-fg"
          aria-label={m.verdict_contributors()}
        >
          {contributors.map((c) => (
            <ContributorRow key={c.id} c={c} params={params} />
          ))}
        </ul>
      )}
    </section>
  );
}
