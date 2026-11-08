import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { objects } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { ObjectsScreen, ObjectsView } from "./Objects";

/** Every declarative object aimed at the Cluster with its PostgreSQL error, managed roles, tablespaces, poolers and the image catalog. A list that could not be read is empty with its reason in the sources. */
const meta = {
  title: "Screens/Objects",
  component: ObjectsView,
  args: { data: objects },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof ObjectsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Databases (Database objects)" })).toBeVisible();
    await expect(canvas.getAllByText("failed").length).toBeGreaterThan(0);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Empty: Story = {
  args: {
    data: {
      ...objects,
      databases: [],
      roles: [],
      managedRoles: [],
      tablespaces: [],
      publications: [],
      subscriptions: [],
      poolers: [],
      imageCatalog: undefined,
    },
  },
};
const screen = { render: () => <ObjectsScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
export const Forbidden: Story = { ...screen, parameters: { msw: handlersFor("forbidden") } };
