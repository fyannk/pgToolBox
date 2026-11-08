import { createFileRoute } from "@tanstack/react-router";
import { m } from "../paraglide/messages.js";
import { AccessRequestsScreen } from "../screens/AccessRequests";

type Search = { namespace?: string; cluster?: string };

export const Route = createFileRoute("/access")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    namespace: typeof s.namespace === "string" && s.namespace ? s.namespace : undefined,
    cluster: typeof s.cluster === "string" && s.cluster ? s.cluster : undefined,
  }),
  component: AccessRoute,
});

function AccessRoute() {
  const search = Route.useSearch();
  return (
    <>
      <h1 className="font-semibold text-display">{m.nav_access()}</h1>
      <AccessRequestsScreen initial={search} />
    </>
  );
}
