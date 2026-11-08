import { createFileRoute, Outlet } from "@tanstack/react-router";
import { InsightNav } from "../../../../../screens/insight/InsightNav";

export const Route = createFileRoute("/c/$namespace/$cluster/insight")({
  component: InsightLayout,
});

function InsightLayout() {
  const { namespace, cluster } = Route.useParams();
  return (
    <>
      <InsightNav namespace={namespace} cluster={cluster} />
      <Outlet />
    </>
  );
}
