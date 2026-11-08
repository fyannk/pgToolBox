import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent, waitFor } from "storybook/test";
import {
  accessCreateInvalid,
  accessRequests,
  accessRequestsReadOnly,
  accessRequestsRequester,
  clusterList,
} from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { ApiError } from "../api/fetcher";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { AccessRequestsScreen, AccessRequestsView } from "./AccessRequests";

/**
 * The UI over PgAccessRequest on the Go goldens: the
 * request form (for the reader), the reader's requests with phase and
 * decided-by, and the requests the reader may decide.
 */
const meta = {
  title: "Screens/AccessRequests",
  component: AccessRequestsView,
  args: {
    data: accessRequests,
    clusters: (clusterList.items ?? []).map((c) => c.ref),
    actor: "oidc:jane@corp.example",
    onCreate: fn(async () => ({})),
    onDecide: fn(async () => ({})),
  },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof AccessRequestsView>;
export default meta;
type Story = StoryObj<typeof meta>;

/** A reviewer of team-a and team-b. */
export const Reviewer: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "To review" })).toBeVisible();
    await expect(canvas.getByRole("button", { name: /Approve/ })).toBeVisible();
  },
};
export const ReviewerDark = dark(Reviewer);
export const ReviewerCompact = compact(Reviewer);
export const ReviewerDarkCompact = darkCompact(Reviewer);
/** The French catalogue (no play: its assertions read English). */
export const ReviewerFrench: Story = french({});
/** A requester who may not list every request: only those naming them, said so. */
export const Requester: Story = {
  args: { data: accessRequestsRequester },
  play: async ({ canvas }) => {
    await expect(canvas.queryByRole("heading", { name: "To review" })).toBeNull();
  },
};
/** The console does not write (OPERATIONS_ENABLED off): no form, no decision. */
export const ReadOnly: Story = {
  args: { data: accessRequestsReadOnly },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/This console does not write/)).toBeVisible();
    await expect(canvas.queryByRole("button")).toBeNull();
  },
};
export const ReadOnlyDark = dark(ReadOnly);
export const Empty: Story = {
  args: { data: { ...accessRequests, items: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("You have no access request.")).toBeVisible();
    await expect(canvas.getByText("Nothing waits for your review.")).toBeVisible();
  },
};
export const NothingToRequest: Story = {
  args: {
    data: {
      ...accessRequests,
      namespaces: (accessRequests.namespaces ?? []).map((n) => ({ ...n, canRequest: false })),
    },
  },
};

/** The form sends only what was chosen and says nothing is granted yet. */
export const CreateFlow: Story = {
  args: { initial: { namespace: "team-a", cluster: "orders" } },
  play: async ({ args, canvas }) => {
    const send = canvas.getByRole("button", { name: "Send the request" });
    await expect(send).toBeDisabled();
    await userEvent.type(canvas.getByLabelText(/^Justification/), "INC-4300: failover drill");
    await userEvent.click(send);
    await expect(args.onCreate).toHaveBeenCalledWith("team-a", {
      capability: "viewer",
      cluster: "orders",
      justification: "INC-4300: failover drill",
    });
    await waitFor(() => expect(canvas.getByRole("status")).toHaveTextContent(/Nothing is granted/));
  },
};
/** A refused request (422 from the Go golden) stays on the form with the server's reason and request id. */
export const CreateRefused: Story = {
  args: {
    initial: { namespace: "team-a" },
    onCreate: fn(async () => {
      throw new ApiError(422, accessCreateInvalid, "req-12");
    }),
  },
  play: async ({ canvas }) => {
    await userEvent.type(canvas.getByLabelText(/^Justification/), "x");
    await userEvent.click(canvas.getByRole("button", { name: "Send the request" }));
    await waitFor(() => expect(canvas.getByRole("alert")).toHaveTextContent("req-12"));
    await expect(canvas.getByRole("alert")).toHaveTextContent(/must be in the future/);
  },
};
const screen = { render: () => <AccessRequestsScreen /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { ...screen, parameters: { msw: handlersFor("unavailable") } };
