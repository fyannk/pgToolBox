import { createFileRoute, notFound } from "@tanstack/react-router";
import { RevisionScreen } from "../../../../../screens/History";

export const Route = createFileRoute("/c/$namespace/$cluster/history/$revision")({
  // A revision is a sequence number; anything else is no address of this console.
  params: {
    parse: (p) => {
      if (!/^\d+$/.test(p.revision)) throw notFound();
      return { revision: Number(p.revision) };
    },
    stringify: (p) => ({ revision: String(p.revision) }),
  },
  component: () => {
    const { namespace, cluster, revision } = Route.useParams();
    return <RevisionScreen namespace={namespace} cluster={cluster} revision={revision} />;
  },
});
