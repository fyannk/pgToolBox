import { createFileRoute } from "@tanstack/react-router";
import { LogsScreen } from "../../../../../screens/Logs";

type Search = { container?: string; previous?: boolean };

export const Route = createFileRoute("/c/$namespace/$cluster/logs/$pod")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    container: typeof s.container === "string" && s.container ? s.container : undefined,
    previous: s.previous === true || s.previous === "true" ? true : undefined,
  }),
  component: LogsRoute,
});

function LogsRoute() {
  const { namespace, cluster, pod } = Route.useParams();
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <LogsScreen
      namespace={namespace}
      cluster={cluster}
      pod={pod}
      container={search.container}
      previous={search.previous}
      onChange={(next) =>
        void navigate({ search: { container: next.container, previous: next.previous || undefined }, replace: true })
      }
    />
  );
}
