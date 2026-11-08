import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent } from "storybook/test";
import { ApiError } from "../../../api/fetcher";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { EmptyState, ErrorState, LoadingState } from "./States";

/**
 * The states every request has. Loading says what it waits for; Empty is a
 * list read successfully and empty; Error carries the request id and a retry;
 * Forbidden names the role that would allow it.
 */
const meta = { title: "Patterns/States", component: LoadingState, args: { label: "Clusters" } } satisfies Meta<
  typeof LoadingState
>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Loading: Story = {};
export const LoadingDark = dark(Loading);
export const LoadingCompact = compact(Loading);
export const LoadingDarkCompact = darkCompact(Loading);

export const Empty: StoryObj<typeof EmptyState> = {
  render: () => (
    <EmptyState title="No Cluster in the namespaces this console could list.">
      Namespaces that could not be listed are named above.
    </EmptyState>
  ),
};
export const EmptyDark = dark(Empty);

const retry = fn();
export const ServerError: StoryObj<typeof ErrorState> = {
  render: () => (
    <ErrorState
      what="Clusters"
      error={
        new ApiError(
          503,
          {
            title: "Service Unavailable",
            status: 503,
            detail: "the read model could not answer; the console log has the cause under reference 3f9c2a7d1e4b",
          },
          "req-0001",
        )
      }
      onRetry={retry}
    />
  ),
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("alert")).toHaveTextContent("Request id req-0001");
    await expect(canvas.getByRole("alert")).toHaveTextContent("under reference 3f9c2a7d1e4b");
    await userEvent.click(canvas.getByRole("button", { name: "Retry" }));
    await expect(retry).toHaveBeenCalled();
  },
};
export const ServerErrorDark = dark(ServerError);

export const Forbidden: StoryObj<typeof ErrorState> = {
  render: () => (
    <ErrorState
      what="Diagnostics"
      error={
        new ApiError(
          403,
          {
            title: "Forbidden",
            status: 403,
            detail:
              "you may not get clusters.postgresql.cnpg.io/billing in namespace team-a: ask for the team-viewer role there",
          },
          null,
        )
      }
    />
  ),
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("alert")).toHaveTextContent(/ask for the team-viewer role there/);
  },
};
export const ForbiddenDark = dark(Forbidden);

export const Reauth: StoryObj<typeof ErrorState> = {
  render: () => <ErrorState what="Events" error={new ApiError(401, { title: "Unauthorized", status: 401 }, null)} />,
};
