import { createFileRoute } from "@tanstack/react-router";
import { CompareScreen } from "../screens/Compare";

type Search = { left?: string; right?: string; diff?: boolean };

const key = (v: unknown) => (typeof v === "string" && v.includes("/") ? v : undefined);

export const Route = createFileRoute("/compare")({
  validateSearch: (s: Record<string, unknown>): Search => ({
    left: key(s.left),
    right: key(s.right),
    diff: s.diff === true || s.diff === "true" ? true : undefined,
  }),
  component: CompareRoute,
});

function CompareRoute() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <CompareScreen
      left={search.left}
      right={search.right}
      onlyDifferences={search.diff === true}
      onSearchChange={(next) => void navigate({ search: (prev) => ({ ...prev, ...next }), replace: true })}
    />
  );
}
