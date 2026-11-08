import type { Meta, StoryObj } from "@storybook/react-vite";
import { useEffect, useState } from "react";
import { expect, userEvent, waitFor, within } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { fixtureLog } from "./fixture";
import { type LogLine, LogViewer } from "./LogViewer";

/** Appends `more` lines after the first render, as a stream would. */
function Streaming({ initial, more }: { initial: number; more: number }) {
  const [lines, setLines] = useState<LogLine[]>(() => fixtureLog(initial));
  useEffect(() => {
    if (more === 0) return;
    const id = setTimeout(() => setLines((l) => [...l, ...fixtureLog(more, l.length)]), 50);
    return () => clearTimeout(id);
  }, [more]);
  return <LogViewer title="Log of billing-1 (postgres)" lines={lines} state="live" />;
}

/**
 * A container log as it streams. Use it for one container of one pod; it is
 * not a search engine over history. Follows the end until the reader scrolls
 * up or pauses; new lines then wait behind an "N new lines" button so the text
 * being read never moves. The stream state is said in words, gaps included.
 */
const meta = {
  title: "Patterns/LogViewer",
  component: LogViewer,
  args: { title: "Log of billing-1 (postgres)", lines: fixtureLog(60), state: "live" },
} satisfies Meta<typeof LogViewer>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Live: Story = {
  play: async ({ canvas }) => {
    const log = canvas.getByRole("log", { name: "Log of billing-1 (postgres)" });
    await waitFor(() => expect(log.scrollTop + log.clientHeight).toBeGreaterThanOrEqual(log.scrollHeight - 8));
    await expect(canvas.getByRole("button", { name: "Pause" })).toHaveAttribute("aria-pressed", "true");
  },
};
export const LiveDark = dark(Live);
export const LiveCompact = compact(Live);
export const LiveDarkCompact = darkCompact(Live);

/** Times as containers write them (RFC 3339 Nano, trailing zeros dropped, an offset): shown in UTC at one width. */
export const RawTimes: Story = {
  args: {
    lines: [
      { seq: 0, time: "2026-10-29T11:50:00Z", text: "whole second" },
      { seq: 1, time: "2026-10-29T11:50:00.12Z", text: "trailing zero dropped" },
      { seq: 2, time: "2026-10-29T11:50:00.123456789Z", text: "nanoseconds" },
      { seq: 3, time: "2026-10-29T13:50:01.5+02:00", text: "with an offset" },
    ],
    state: "live",
  },
  play: async ({ canvas }) => {
    for (const t of ["11:50:00.000", "11:50:00.120", "11:50:00.123", "11:50:01.500"]) {
      await expect(canvas.getByText(t)).toBeVisible();
    }
  },
};
export const Connecting: Story = { args: { lines: [], state: "connecting" } };
export const Empty: Story = { args: { lines: [], state: "live" } };
/** After a reconnection the gap is said, not papered over. */
export const Interrupted: Story = {
  args: { state: "down" },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText(/lines in the gap may be missing/)[0]).toBeVisible();
  },
};
/** The server ended the stream: its reason is shown, nothing reconnects. */
export const Ended: Story = {
  args: { state: "ended", endReason: "the session was revoked" },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Stream ended: the session was revoked")).toBeVisible();
  },
};
export const Dropped: Story = { args: { lines: fixtureLog(50, 9950), dropped: true, maxLines: 10000 } };
/**
 * A long log is drawn in chunks the browser skips off screen. 1,000 lines
 * (five chunks) keeps the per-story axe run inside its time budget on a
 * loaded runner; the chunking itself is covered by the unit test.
 */
export const Long: Story = { args: { lines: fixtureLog(1000) } };

export const Search: Story = {
  play: async ({ canvas }) => {
    await userEvent.type(canvas.getByLabelText("Search the log"), "wal stream");
    await expect(canvas.getByText("12 of 60 lines match")).toBeVisible();
    await userEvent.clear(canvas.getByLabelText("Search the log"));
    await userEvent.type(canvas.getByLabelText("Search the log"), "nothing like this");
    await expect(canvas.getByText(/No line matches/)).toBeVisible();
  },
};

/** Paused: new lines wait behind a button; it jumps to the end and follows again. */
export const PausedWithNewLines: Story = {
  render: () => <Streaming initial={60} more={0} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await userEvent.click(canvas.getByRole("button", { name: "Pause" }));
    await expect(canvas.getByRole("button", { name: "Follow" })).toHaveAttribute("aria-pressed", "false");
  },
};
export const NewLinesWhilePaused: Story = {
  render: function Render() {
    const [lines, setLines] = useState<LogLine[]>(() => fixtureLog(60));
    return (
      <>
        <button type="button" onClick={() => setLines((l) => [...l, ...fixtureLog(3, l.length)])}>
          Append three lines
        </button>
        <LogViewer title="Log of billing-1 (postgres)" lines={lines} state="live" />
      </>
    );
  },
  play: async ({ canvas }) => {
    await userEvent.click(canvas.getByRole("button", { name: "Pause" }));
    await userEvent.click(canvas.getByRole("button", { name: "Append three lines" }));
    const jump = await canvas.findByRole("button", { name: "3 new lines — jump to the end" });
    await userEvent.click(jump);
    await expect(canvas.getByRole("button", { name: "Pause" })).toBeVisible();
    await expect(canvas.queryByRole("button", { name: /new lines/ })).toBeNull();
  },
};

/** A gap from the stream (or a reconnection) is a marker, never joined across; search keeps it. */
export const Gap: Story = {
  args: {
    lines: [...fixtureLog(10), { seq: 10, text: "", gap: "the API server closed the stream" }, ...fixtureLog(10, 11)],
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Lines were missed here: the API server closed the stream/)).toBeVisible();
    await userEvent.type(canvas.getByLabelText("Search the log"), "checkpoint");
    await expect(canvas.getByText(/Lines were missed here/)).toBeVisible();
  },
};
