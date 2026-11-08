import { createFileRoute } from "@tanstack/react-router";
import { IndexesScreen } from "../../../../../screens/insight/Indexes";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/indexes")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <IndexesScreen namespace={namespace} cluster={cluster} />;
  },
});
