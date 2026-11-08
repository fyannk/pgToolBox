import { createFileRoute, Outlet } from "@tanstack/react-router";
import { ClusterFrame } from "../../../../screens/ClusterFrame";

export const Route = createFileRoute("/c/$namespace/$cluster")({
  component: ClusterLayout,
});

function ClusterLayout() {
  const { namespace, cluster } = Route.useParams();
  return (
    <ClusterFrame namespace={namespace} cluster={cluster}>
      <Outlet />
    </ClusterFrame>
  );
}
