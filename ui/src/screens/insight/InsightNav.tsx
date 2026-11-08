import { Link } from "@tanstack/react-router";
import { m } from "../../paraglide/messages.js";

/** The insight screens of one Cluster, in reading order. */
const INSIGHT_SCREENS = [
  { to: "/c/$namespace/$cluster/insight", label: () => m.insight_nav_pack(), exact: true },
  { to: "/c/$namespace/$cluster/insight/vacuum", label: () => m.insight_nav_vacuum(), exact: false },
  { to: "/c/$namespace/$cluster/insight/blocking", label: () => m.insight_nav_blocking(), exact: false },
  { to: "/c/$namespace/$cluster/insight/statements", label: () => m.insight_nav_statements(), exact: false },
  { to: "/c/$namespace/$cluster/insight/indexes", label: () => m.insight_nav_indexes(), exact: false },
  { to: "/c/$namespace/$cluster/insight/connections", label: () => m.insight_nav_connections(), exact: false },
] as const;

/** The second-level navigation of the Insight section. */
export function InsightNav({ namespace, cluster }: { namespace: string; cluster: string }) {
  return (
    <nav aria-label={m.insight_screens()}>
      <ul className="flex flex-wrap gap-1">
        {INSIGHT_SCREENS.map((s) => (
          <li key={s.to}>
            <Link
              to={s.to}
              params={{ namespace, cluster }}
              activeOptions={{ exact: s.exact }}
              className="block rounded-control border border-line px-2 py-1 text-label text-muted hover:text-fg data-[status=active]:border-accent data-[status=active]:font-semibold data-[status=active]:text-fg"
            >
              {s.label()}
            </Link>
          </li>
        ))}
      </ul>
    </nav>
  );
}
