import { createFileRoute } from "@tanstack/react-router";

/** The table without an open drawer; the layout renders the screen. */
export const Route = createFileRoute("/c/$namespace/$cluster/instances/")({ component: () => null });
