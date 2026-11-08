import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { HistoryScreen } from "../../../../../screens/History";
import { validateHistorySearch } from "../../../../../screens/historySearch";

export const Route = createFileRoute("/c/$namespace/$cluster/history/")({
  // The tab, the timeline's window and kinds are in the URL: a view is a link.
  validateSearch: validateHistorySearch,
  component: HistoryRoute,
});

function HistoryRoute() {
  const { namespace, cluster } = Route.useParams();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });
  return (
    <HistoryScreen
      namespace={namespace}
      cluster={cluster}
      search={search}
      onSearch={(next) => void navigate({ search: next })}
    />
  );
}
