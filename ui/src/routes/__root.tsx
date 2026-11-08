import type { QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext } from "@tanstack/react-router";
import { Shell } from "../app/Shell";
import { NotFound } from "../screens/NotFound";

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Shell,
  notFoundComponent: NotFound,
});
