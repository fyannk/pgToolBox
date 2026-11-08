import { createFileRoute } from "@tanstack/react-router";
import { OperationsScreen } from "../../../../screens/Operations";

export const Route = createFileRoute("/c/$namespace/$cluster/operations")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <OperationsScreen namespace={namespace} cluster={cluster} />;
  },
});
