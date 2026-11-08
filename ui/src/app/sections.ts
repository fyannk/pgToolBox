import { useGetSession } from "../api/generated/endpoints/session/session";
import type { Features } from "../api/generated/model";
import { m } from "../paraglide/messages.js";

/** The sections of one Cluster, in navigation order: the tabs and the palette share it. */
const CLUSTER_SECTIONS = [
  { to: "/c/$namespace/$cluster", label: () => m.nav_overview(), exact: true },
  { to: "/c/$namespace/$cluster/instances", label: () => m.nav_instances(), exact: false },
  { to: "/c/$namespace/$cluster/replication", label: () => m.nav_replication(), exact: false },
  { to: "/c/$namespace/$cluster/backups", label: () => m.nav_backups(), exact: false },
  { to: "/c/$namespace/$cluster/storage", label: () => m.nav_storage(), exact: false },
  { to: "/c/$namespace/$cluster/objects", label: () => m.nav_objects(), exact: false },
  { to: "/c/$namespace/$cluster/diagnostics", label: () => m.nav_diagnostics(), exact: false },
  { to: "/c/$namespace/$cluster/events", label: () => m.nav_events(), exact: false },
  { to: "/c/$namespace/$cluster/history", label: () => m.nav_history(), exact: false, feature: "history" },
  { to: "/c/$namespace/$cluster/metrics", label: () => m.nav_metrics(), exact: false, feature: "metrics" },
  // Not gated on the metrics feature: with scraping off the screen says so (pack state metricsOff).
  { to: "/c/$namespace/$cluster/insight", label: () => m.nav_insight(), exact: false },
  { to: "/c/$namespace/$cluster/operations", label: () => m.nav_operations(), exact: false, feature: "operations" },
] as const satisfies ReadonlyArray<{ to: string; label: () => string; exact: boolean; feature?: keyof Features }>;

/** What this console serves at all (session.features); everything until the session is read. */
export function useFeatures(): Features | null {
  const session = useGetSession({ query: { retry: false, refetchInterval: false } });
  return session.data?.features ?? null;
}

/** The sections this console serves: one whose feature is switched off is not offered. */
export function useClusterSections() {
  const features = useFeatures();
  return CLUSTER_SECTIONS.filter((s) => !("feature" in s) || !features || features[s.feature]);
}
