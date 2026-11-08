import { createFileRoute } from "@tanstack/react-router";
import { m } from "../../paraglide/messages.js";
import { DigestScreen } from "../../screens/Digests";

// The page a delivered digest links to (PUBLIC_URL/digests/<id>).
export const Route = createFileRoute("/digests/$id")({
  component: DigestRoute,
});

function DigestRoute() {
  const { id } = Route.useParams();
  return (
    <>
      <h1 className="font-semibold text-display">{m.digest_heading({ id })}</h1>
      <DigestScreen id={id} />
    </>
  );
}
