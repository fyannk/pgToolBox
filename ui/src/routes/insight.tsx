import { createFileRoute } from "@tanstack/react-router";
import { m } from "../paraglide/messages.js";
import { FleetStatementsScreen } from "../screens/insight/Fleet";

export const Route = createFileRoute("/insight")({
  component: () => (
    <>
      <h1 className="font-semibold text-display">{m.nav_fleet_insight()}</h1>
      <FleetStatementsScreen />
    </>
  ),
});
