import { createFileRoute } from "@tanstack/react-router";
import { EventsScreen } from "../../../../screens/Events";

export const Route = createFileRoute("/c/$namespace/$cluster/events")({
  component: () => {
    const { namespace, cluster } = Route.useParams();
    return <EventsScreen namespace={namespace} cluster={cluster} />;
  },
});
