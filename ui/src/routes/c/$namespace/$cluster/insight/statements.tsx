import { createFileRoute } from "@tanstack/react-router";
import { StatementsScreen } from "../../../../../screens/insight/Statements";

export const Route = createFileRoute("/c/$namespace/$cluster/insight/statements")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <StatementsScreen namespace={namespace} cluster={cluster} />;
  },
});
