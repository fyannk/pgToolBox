import { createFileRoute } from "@tanstack/react-router";
import { ClusterListScreen } from "../screens/ClusterList";

type Search = { q?: string; sort?: string };

export const Route = createFileRoute("/")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    q: typeof s.q === "string" && s.q ? s.q : undefined,
    sort: typeof s.sort === "string" && s.sort ? s.sort : undefined,
  }),
  component: ClusterListRoute,
});

function ClusterListRoute() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <ClusterListScreen
      q={search.q ?? ""}
      sort={search.sort}
      onSearchChange={(next) => void navigate({ search: (prev) => ({ ...prev, ...next }), replace: true })}
    />
  );
}
