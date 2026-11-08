import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { storage, storageForecasts } from "../../mocks/fixtures";
import {
  storageArchivingFailing,
  storageAutoGrow,
  storageForecastsDisabled,
  storageTooFewPoints,
} from "../../mocks/fixtures/extra";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { StorageScreen, StorageView } from "./Storage";

/**
 * What the Cluster asks for (with the fork's autoGrow), the claims that exist in the operator's groups, usage only
 * where an instance reports it, and the volume forecasts: on stock CloudNativePG the panel says why there are none
 * (Go's reason, detected from the data); on a build that reports its volumes, each volume's forecast is a sentence
 * with its date range, rate, window and confidence, never a bare date.
 */
const meta = {
  title: "Screens/Storage",
  component: StorageView,
  args: { data: storage },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof StorageView>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Stock CloudNativePG (orders): no instance reports its volumes, so no forecast and Go says why. */
export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Persistent volume claims" })).toBeVisible();
    await expect(canvas.getByText("No volume forecast for this Cluster")).toBeVisible();
    await expect(canvas.getByText(/Upstream CloudNativePG reports neither/)).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Empty: Story = {
  args: { data: { ...storage, volumes: [], requests: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No PVC observed for this Cluster.")).toBeVisible();
  },
};
const screen = { render: () => <StorageScreen namespace="team-a" cluster="orders" /> };
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

/** A build that reports its volumes (billing): every forecast state of the golden at once. */
export const Forecasts: Story = {
  args: { data: storageForecasts },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Volume forecasts" })).toBeVisible();
    await expect(canvas.getByText(/^Reaches autoGrow\.maxSize \(100Gi\) in about 46 days/)).toBeVisible();
    await expect(canvas.getByText("up to 100Gi, step 20% at 80% used")).toBeVisible();
    await expect(canvas.getByText(/It survives console restarts/)).toBeVisible();
  },
};
export const ForecastsDark = dark(Forecasts);
export const ForecastsCompact = compact(Forecasts);
export const ForecastsDarkCompact = darkCompact(Forecasts);
export const ForecastsFrench = french({ args: { data: storageForecasts } } satisfies Story);
/** A replica created nine hours ago: too few hourly points, said with the count. */
export const TooFewPoints: Story = {
  args: { data: storageTooFewPoints },
  play: async ({ canvas }) => {
    // With autoGrow, the filesystem and the maxSize forecast each say so.
    const lines = canvas.getAllByText("Too few points to forecast: 9 of 24 hourly points.");
    await expect(lines).toHaveLength(2);
    for (const line of lines) await expect(line).toBeVisible();
    await expect(canvas.getByText("Inode counts are not reported by this filesystem.")).toBeVisible();
  },
};
/** Toward autoGrow.maxSize: the operator steps the PVC before the date; the trend has a gap, drawn as one. */
export const AutoGrow: Story = {
  args: { data: storageAutoGrow },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/autoGrow will grow the PVC in steps before then/)).toBeVisible();
    await expect(canvas.getAllByText("Toward autoGrow.maxSize").length).toBeGreaterThan(0);
    await expect(canvas.getByText(/Breaks in the line are gaps/)).toBeVisible();
  },
};
export const AutoGrowDark = dark(AutoGrow);
/** WAL piling up while archiving fails: the warning comes before the forecast, which is accelerating. */
export const ArchivingFailing: Story = {
  args: { data: storageArchivingFailing },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("WAL archiving failing")).toBeVisible();
    await expect(canvas.getByText(/the growth is a symptom, fix archiving first/)).toBeVisible();
    await expect(canvas.getByText(/^Growing faster than linear/)).toBeVisible();
  },
};
export const ArchivingFailingDarkCompact = darkCompact(ArchivingFailing);
/** The console keeps no trend (configuration). */
export const ForecastsDisabled: Story = {
  args: { data: storageForecastsDisabled },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Volume forecasts are disabled")).toBeVisible();
  },
};
const billingScreen = { render: () => <StorageScreen namespace="team-a" cluster="billing" /> };
/** The billing Cluster through the API (MSW): the forecasts arrive with the rest of the view. */
export const LiveForecasts: Story = {
  ...billingScreen,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("heading", { name: "Volume forecasts" })).toBeVisible();
  },
};
