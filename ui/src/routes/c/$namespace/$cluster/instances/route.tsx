import { createFileRoute, Outlet, useParams } from "@tanstack/react-router";
import { InstancesScreen } from "../../../../../screens/Instances";

/**
 * /instances and /instances/<pod> share one screen: the pod in
 * the URL is the instance whose drawer is open. The layout stays mounted when
 * the pod changes, so the table keeps its state and focus returns to the row.
 */
export const Route = createFileRoute("/c/$namespace/$cluster/instances")({
  component: InstancesLayout,
});

function InstancesLayout() {
  const { namespace, cluster } = Route.useParams();
  const { pod } = useParams({ strict: false });
  const navigate = Route.useNavigate();
  return (
    <>
      <InstancesScreen
        namespace={namespace}
        cluster={cluster}
        pod={pod}
        onPodChange={(next) => {
          if (next)
            void navigate({ to: "/c/$namespace/$cluster/instances/$pod", params: { namespace, cluster, pod: next } });
          else void navigate({ to: "/c/$namespace/$cluster/instances", params: { namespace, cluster } });
        }}
      />
      <Outlet />
    </>
  );
}
