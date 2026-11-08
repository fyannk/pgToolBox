import { createFileRoute } from "@tanstack/react-router";
import { OverviewScreen } from "../../../../screens/Overview";

export const Route = createFileRoute("/c/$namespace/$cluster/")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <OverviewScreen namespace={namespace} cluster={cluster} />;
  },
});
