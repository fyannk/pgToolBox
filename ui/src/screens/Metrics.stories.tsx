import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { metrics } from "../../mocks/fixtures";
import { metricsPrometheus, metricsPrometheusDown } from "../../mocks/fixtures/extra";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { MetricsScreen, MetricsView } from "./Metrics";

/** The latest readings per instance and the console's short ring as charts; it is not a monitoring system and links out to the one the site runs. */
const meta = {
  title: "Screens/Metrics",
  component: MetricsView,
  args: { data: metrics, params: { namespace: "team-a", cluster: "orders" } },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof MetricsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("img", { name: "Replication lag" })).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Empty: Story = { args: { data: { ...metrics, tiles: [], charts: [] } } };
const screen = { render: () => <MetricsScreen namespace="team-a" cluster="orders" /> };
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

/** Go's chartSource is prometheus: the charts are the configured Prometheus over rangeSeconds at stepSeconds. */
export const FromPrometheus: Story = {
  args: { data: metricsPrometheus },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/From Prometheus over the last 6 h/)).toBeVisible();
  },
};
export const FromPrometheusDark = dark(FromPrometheus);
/** Prometheus configured but not answering: Go's SourceState health and its reason for the fallback to the ring. */
export const PrometheusDown: Story = {
  args: { data: metricsPrometheusDown },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Prometheus did not answer/)).toBeVisible();
  },
};
export const PrometheusDownDarkCompact = darkCompact(PrometheusDown);
