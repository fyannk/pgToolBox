import { createFileRoute } from "@tanstack/react-router";
import { ConnectionsScreen } from "../../../../../screens/insight/Connections";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/connections")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <ConnectionsScreen namespace={namespace} cluster={cluster} />;
  },
});
