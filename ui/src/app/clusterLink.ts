// Cluster-relative links from the Go view models (Contributor.link,
// Finding.link) resolved onto the routes this build has. Go owns the link
// (internal/console/status link() maps the catalogue's paths); the UI only
// knows its own route table. A link to a section this build does not have is
// rendered as plain text, never as a dead link and never guessed onto another
// screen.

export type ClusterParams = { namespace: string; cluster: string };

/** The cluster sections that take no further segment. */
const SECTIONS = {
  events: "/c/$namespace/$cluster/events",
  diagnostics: "/c/$namespace/$cluster/diagnostics",
  triage: "/c/$namespace/$cluster/diagnostics",
  replication: "/c/$namespace/$cluster/replication",
  backups: "/c/$namespace/$cluster/backups",
  storage: "/c/$namespace/$cluster/storage",
  objects: "/c/$namespace/$cluster/objects",
  metrics: "/c/$namespace/$cluster/metrics",
  operations: "/c/$namespace/$cluster/operations",
} as const;
type Section = (typeof SECTIONS)[keyof typeof SECTIONS];

/** The insight screens a link may name as insight/<screen> (Go's link mapper uses the same set). */
const INSIGHT = {
  vacuum: "/c/$namespace/$cluster/insight/vacuum",
  blocking: "/c/$namespace/$cluster/insight/blocking",
  statements: "/c/$namespace/$cluster/insight/statements",
  indexes: "/c/$namespace/$cluster/insight/indexes",
  connections: "/c/$namespace/$cluster/insight/connections",
} as const;
type InsightScreen = (typeof INSIGHT)[keyof typeof INSIGHT];

export type ClusterTarget =
  | { to: "/c/$namespace/$cluster"; params: ClusterParams }
  | { to: "/c/$namespace/$cluster/instances"; params: ClusterParams }
  | { to: "/c/$namespace/$cluster/instances/$pod"; params: ClusterParams & { pod: string } }
  | { to: "/c/$namespace/$cluster/logs/$pod"; params: ClusterParams & { pod: string } }
  | { to: "/c/$namespace/$cluster/history"; params: ClusterParams }
  | { to: "/c/$namespace/$cluster/history/$revision"; params: ClusterParams & { revision: number } }
  | { to: "/c/$namespace/$cluster/insight"; params: ClusterParams }
  | { to: InsightScreen; params: ClusterParams }
  | { to: Section; params: ClusterParams };

/** The route of a Go link, or null when this build has no such screen. */
export function resolveClusterLink(link: string | null | undefined, params: ClusterParams): ClusterTarget | null {
  if (link === undefined || link === null) return null;
  const [section = "", rest = "", ...extra] = link.replace(/^\/+|\/+$/g, "").split("/");
  if (extra.length > 0) return null;
  switch (section) {
    case "":
      return { to: "/c/$namespace/$cluster", params };
    case "instances":
      return rest
        ? { to: "/c/$namespace/$cluster/instances/$pod", params: { ...params, pod: rest } }
        : { to: "/c/$namespace/$cluster/instances", params };
    case "logs":
      return rest ? { to: "/c/$namespace/$cluster/logs/$pod", params: { ...params, pod: rest } } : null;
    case "history":
      if (!rest) return { to: "/c/$namespace/$cluster/history", params };
      return /^\d+$/.test(rest)
        ? { to: "/c/$namespace/$cluster/history/$revision", params: { ...params, revision: Number(rest) } }
        : null;
    case "insight":
      if (!rest) return { to: "/c/$namespace/$cluster/insight", params };
      return Object.hasOwn(INSIGHT, rest) ? { to: INSIGHT[rest as keyof typeof INSIGHT], params } : null;
    default:
      if (rest || !Object.hasOwn(SECTIONS, section)) return null;
      return { to: SECTIONS[section as keyof typeof SECTIONS], params };
  }
}
