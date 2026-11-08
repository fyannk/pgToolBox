import { Link } from "@tanstack/react-router";
import { EmptyState } from "../ds/patterns/States/States";
import { m } from "../paraglide/messages.js";

export function NotFound() {
  return (
    <>
      <h1 className="font-semibold text-display">{m.not_found_title()}</h1>
      <EmptyState title={m.not_found_detail()}>
        <Link to="/" className="text-accent underline">
          {m.back_to_clusters()}
        </Link>
      </EmptyState>
    </>
  );
}
