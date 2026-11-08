import { createFileRoute } from "@tanstack/react-router";
import { m } from "../paraglide/messages.js";
import { AdminScreen } from "../screens/Admin";

export const Route = createFileRoute("/admin")({
  component: () => (
    <>
      <h1 className="font-semibold text-display">{m.nav_admin()}</h1>
      <AdminScreen />
    </>
  ),
});
