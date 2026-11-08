import { createFileRoute } from "@tanstack/react-router";
import { MetricsScreen } from "../../../../screens/Metrics";

export const Route = createFileRoute("/c/$namespace/$cluster/metrics")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <MetricsScreen namespace={namespace} cluster={cluster} />;
  },
});
