import { createFileRoute } from "@tanstack/react-router";
import { ObjectsScreen } from "../../../../screens/Objects";

export const Route = createFileRoute("/c/$namespace/$cluster/objects")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <ObjectsScreen namespace={namespace} cluster={cluster} />;
  },
});
