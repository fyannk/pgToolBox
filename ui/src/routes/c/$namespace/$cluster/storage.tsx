import { createFileRoute } from "@tanstack/react-router";
import { StorageScreen } from "../../../../screens/Storage";

export const Route = createFileRoute("/c/$namespace/$cluster/storage")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <StorageScreen namespace={namespace} cluster={cluster} />;
  },
});
