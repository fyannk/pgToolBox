import { createFileRoute } from "@tanstack/react-router";
import { InsightPackScreen } from "../../../../../screens/insight/Pack";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <InsightPackScreen namespace={namespace} cluster={cluster} />;
  },
});
