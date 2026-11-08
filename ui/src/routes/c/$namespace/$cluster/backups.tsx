import { createFileRoute } from "@tanstack/react-router";
import { BackupsScreen } from "../../../../screens/Backups";

export const Route = createFileRoute("/c/$namespace/$cluster/backups")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <BackupsScreen namespace={namespace} cluster={cluster} />;
  },
});
