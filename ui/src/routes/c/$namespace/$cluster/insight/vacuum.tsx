import { createFileRoute } from "@tanstack/react-router";
import { VacuumScreen } from "../../../../../screens/insight/Vacuum";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/vacuum")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <VacuumScreen namespace={namespace} cluster={cluster} />;
  },
});
