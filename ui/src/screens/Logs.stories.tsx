import type { Meta, StoryObj } from "@storybook/react-vite";
import { HttpResponse, http } from "msw";
import { expect } from "storybook/test";
import { healthyOverview, logTailForbidden, session } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { getGetClusterMockHandler } from "../api/generated/endpoints/clusters/clusters.msw";
import { getGetSessionMockHandler } from "../api/generated/endpoints/session/session.msw";
import { dark } from "../testing/variants";
import { LogsScreen } from "./Logs";

/**
 * One instance's log: the tail read checks access, then the log streams live.
 * Without the right to read logs the server's 403 says which role to ask for.
 */
const meta = {
  title: "Screens/Logs",
  component: LogsScreen,
  args: { namespace: "team-a", cluster: "orders", pod: "orders-1", onChange: () => {} },
  parameters: { layout: "fullscreen", msw: handlersFor("default") },
} satisfies Meta<typeof LogsScreen>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Live: Story = {
  play: async ({ canvas }) => {
    await expect(await canvas.findByText(/checkpoint complete/)).toBeVisible();
  },
};
export const LiveDark = dark(Live);
export const Previous: Story = { args: { previous: true } };
export const Forbidden: Story = {
  parameters: {
    msw: [
      http.get("*/api/v1/namespaces/*/clusters/*/instances/*/logs", () =>
        HttpResponse.json(logTailForbidden, { status: 403, headers: { "Content-Type": "application/problem+json" } }),
      ),
    ],
  },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toBeVisible();
  },
};
export const Loading: Story = { parameters: { msw: handlersFor("loading") } };

/** The console is installed without log access: said, never a blank panel. */
export const FeatureOff: Story = {
  parameters: {
    msw: [
      getGetSessionMockHandler({ ...session, features: { ...session.features, logs: false } }),
      ...handlersFor("default"),
    ],
  },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText(/ALLOW_LOGS is off/)).toBeVisible();
  },
};
/** The API server does not let this reader read pod logs. */
export const NotAllowed: Story = {
  parameters: {
    msw: [
      getGetClusterMockHandler({ ...healthyOverview, capabilities: { ...healthyOverview.capabilities, logs: false } }),
      ...handlersFor("default"),
    ],
  },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText(/get pods\/log/)).toBeVisible();
  },
};
