import { createFileRoute } from "@tanstack/react-router";
import { m } from "../../paraglide/messages.js";
import { DigestsScreen } from "../../screens/Digests";

export const Route = createFileRoute("/digests/")({
  component: () => (
    <>
      <h1 className="font-semibold text-display">{m.nav_digests()}</h1>
      <DigestsScreen />
    </>
  ),
});
