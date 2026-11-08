import { createFileRoute } from "@tanstack/react-router";
import { ReplicationScreen } from "../../../../screens/Replication";

export const Route = createFileRoute("/c/$namespace/$cluster/replication")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <ReplicationScreen namespace={namespace} cluster={cluster} />;
  },
});
