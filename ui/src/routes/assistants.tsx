import { createFileRoute } from "@tanstack/react-router";
import { m } from "../paraglide/messages.js";
import { AssistantsScreen } from "../screens/Assistants";

export const Route = createFileRoute("/assistants")({
  component: () => (
    <>
      <h1 className="font-semibold text-display">{m.nav_assistants()}</h1>
      <AssistantsScreen />
    </>
  ),
});
