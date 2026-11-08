import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import { databaseRoleCreated, databaseRoleExists, databaseRoleUnavailable } from "../../mocks/fixtures";
import { ApiError } from "../api/fetcher";
import { compact, dark, darkCompact } from "../testing/variants";
import { PersonalServerAction } from "./PersonalServer";

/**
 * "My personal server" under the pgAdmin link-out of the Overview: a
 * DatabaseRole of the reader's own (POST …/database-roles). A 201 says the
 * role was declared; it is visible in pgAdmin under "PgToolBox (personal)"
 * only once the registration has run; Go's note says what happens next. The
 * Go goldens (createDatabaseRole*).
 */
const meta = {
  title: "Screens/PersonalServer",
  component: PersonalServerAction,
  args: { cluster: "orders", actor: "oidc:jane@corp.example", onCreate: fn(async () => databaseRoleCreated) },
} satisfies Meta<typeof PersonalServerAction>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Offer: Story = {};
export const OfferDark = dark(Offer);
export const OfferCompact = compact(Offer);
/** Role name and typed Cluster name; the answer says created is not yet visible. */
export const Declared: Story = {
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Create my personal server/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await expect(within(dialog).getByRole("button", { name: "Request" })).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/^PostgreSQL role/), "alice_ro");
    await userEvent.type(within(dialog).getByLabelText(/^Member of/), "reporting");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await expect(args.onCreate).toHaveBeenCalledWith({ role: "alice_ro", inRoles: ["reporting"] });
    await waitFor(() => expect(canvas.getByRole("status")).toHaveTextContent(/PgToolBox \(personal\)/));
    await expect(canvas.getByRole("status")).toHaveTextContent(/ServersSynced/);
  },
};
export const DeclaredDarkCompact = darkCompact(Declared);
/** 501 on a CloudNativePG minor without the feature: Go's words, in the dialog. */
export const Unsupported: Story = {
  args: {
    onCreate: fn(async () => {
      throw new ApiError(501, databaseRoleUnavailable, "req-31");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Create my personal server/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/^PostgreSQL role/), "alice_ro");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent(/CloudNativePG 1.31/));
  },
};
/** 409: a DatabaseRole of that name exists; nothing is shown as created. */
export const Exists: Story = {
  args: {
    onCreate: fn(async () => {
      throw new ApiError(409, databaseRoleExists, "req-32");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Create my personal server/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/^PostgreSQL role/), "alice_ro");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent("req-32"));
    await expect(canvas.queryByText(/declared for pgAdmin user/)).toBeNull();
  },
};
