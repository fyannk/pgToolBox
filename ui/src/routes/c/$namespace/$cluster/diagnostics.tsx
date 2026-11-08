import { createFileRoute } from "@tanstack/react-router";
import { DiagnosticsScreen } from "../../../../screens/Diagnostics";

export const Route = createFileRoute("/c/$namespace/$cluster/diagnostics")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <DiagnosticsScreen namespace={namespace} cluster={cluster} />;
  },
});
