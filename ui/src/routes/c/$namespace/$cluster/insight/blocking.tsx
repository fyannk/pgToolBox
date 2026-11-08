import { createFileRoute } from "@tanstack/react-router";
import { BlockingScreen } from "../../../../../screens/insight/Blocking";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/blocking")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <BlockingScreen namespace={namespace} cluster={cluster} />;
  },
});
