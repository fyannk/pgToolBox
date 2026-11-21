import type { Meta, StoryObj } from "@storybook/react-vite";
import { HttpResponse } from "msw";
import { expect, spyOn, userEvent, waitFor } from "storybook/test";
import { getGetClusterSummaryMockHandler } from "../api/generated/endpoints/clusters/clusters.msw";
import { compact, dark } from "../testing/variants";
import { CopySummary } from "./CopySummary";

const summary = [
  "**Cluster `billing` in namespace `team-a`: critical**",
  "",
  "- Verdict: critical (rests on a stale source)",
  "- Console: https://console.example.com/c/team-a/billing",
  "",
].join("\n");

/**
 * "Copy as Markdown" on the Cluster overview: the console renders the verdict,
 * its findings with their sources and ages and the sources that are not live as
 * Markdown, and the button puts it on the clipboard. Use it where a reader
 * hands the screen's reading to people who do not have the console open; the
 * text is the console's own, never assembled in the browser. Feedback rule:
 * every click ends in one announced outcome (copied, selected for a manual
 * copy, or an error with its request id).
 */
const meta = {
  title: "Screens/Overview/CopySummary",
  component: CopySummary,
  args: { namespace: "team-a", cluster: "billing" },
  parameters: { msw: [getGetClusterSummaryMockHandler(summary)] },
} satisfies Meta<typeof CopySummary>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Copied: Story = {
  play: async ({ canvas }) => {
    // The headless browser may leave a clipboard permission pending: the story stands in for it.
    const write = spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    await userEvent.click(canvas.getByRole("button", { name: /Copy as Markdown: the incident summary of billing/ }));
    await waitFor(() => expect(canvas.getByRole("status")).toHaveTextContent(/Copied/));
    await expect(write).toHaveBeenCalledWith(summary);
  },
};
export const CopiedDark = dark(Copied);
export const CopiedCompact = compact(Copied);

/** The clipboard refused: the summary is shown, selected, to copy by hand. */
export const ClipboardRefused: Story = {
  play: async ({ canvas }) => {
    spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"));
    await userEvent.click(canvas.getByRole("button", { name: /Copy as Markdown: the incident summary/ }));
    await expect(await canvas.findByRole("textbox", { name: /Incident summary of billing/ })).toHaveValue(summary);
  },
};
export const ClipboardRefusedDark = dark(ClipboardRefused);

/** The console could not render it: said so, with the request id for a report. */
export const Failed: Story = {
  parameters: {
    msw: [
      getGetClusterSummaryMockHandler(() => {
        throw HttpResponse.json(
          { title: "Service Unavailable", status: 503, detail: "the read model could not answer" },
          { status: 503, headers: { "Content-Type": "application/problem+json", "X-Request-Id": "req-sum-1" } },
        );
      }),
    ],
  },
  play: async ({ canvas }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Copy as Markdown: the incident summary/ }));
    await waitFor(() => expect(canvas.getByRole("alert")).toHaveTextContent("req-sum-1"));
  },
};
export const FailedDark = dark(Failed);
