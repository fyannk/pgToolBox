import { createFileRoute } from "@tanstack/react-router";

/** One instance's drawer over the table; the layout reads the pod from the URL. */
export const Route = createFileRoute("/c/$namespace/$cluster/instances/$pod")({ component: () => null });
