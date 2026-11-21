import { afterEach, expect, test, vi } from "vitest";
import { renderInFrame } from "../testing/render";
import { useLogStream } from "./logStream";

class FakeSource {
  static all: FakeSource[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  private handlers = new Map<string, (ev: MessageEvent) => void>();
  constructor(readonly url: string) {
    FakeSource.all.push(this);
  }
  addEventListener(type: string, h: (ev: MessageEvent) => void) {
    this.handlers.set(type, h);
  }
  emit(type: string, data: unknown) {
    this.handlers.get(type)?.(new MessageEvent(type, { data: JSON.stringify(data) }));
  }
  close() {
    this.closed = true;
  }
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  FakeSource.all = [];
});

function Probe({ path }: { path: string }) {
  const s = useLogStream(path);
  return (
    <output>
      {s.state}|{s.endReason ?? ""}|{s.lines.map((l) => (l.gap !== undefined ? `[gap:${l.gap}]` : l.text)).join(",")}
    </output>
  );
}

test("end closes the stream with its reason and never reopens it", async () => {
  vi.stubGlobal("EventSource", FakeSource);
  const screen = await renderInFrame(<Probe path="/s" />);
  const es = FakeSource.all[0];
  if (!es) throw new Error("no source");
  es.onopen?.();
  es.emit("line", { text: "a" });
  es.emit("gap", { at: "2026-10-29T12:00:00Z", reason: "missed" });
  es.emit("end", { reason: "the session was revoked" });
  es.onerror?.();
  await expect.element(screen.getByRole("status")).toHaveTextContent("ended|the session was revoked|a,[gap:missed]");
  expect(es.closed).toBe(true);
  expect(FakeSource.all).toHaveLength(1);
});

test("a broken stream is marked as a gap and reopened, then given up after repeated failures", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeSource);
  const screen = await renderInFrame(<Probe path="/s" />);
  FakeSource.all[0]?.onopen?.();
  FakeSource.all[0]?.emit("line", { text: "a" });
  FakeSource.all[0]?.onerror?.();
  await vi.advanceTimersByTimeAsync(2_000);
  expect(FakeSource.all).toHaveLength(2);
  for (let i = 1; i < 5; i++) {
    FakeSource.all[i]?.onerror?.();
    await vi.advanceTimersByTimeAsync(10_000);
  }
  vi.useRealTimers();
  await expect
    .poll(() => screen.getByRole("status").element().textContent)
    .toContain("ended|the stream could not be reopened");
  // One marker for the whole outage, not one per attempt.
  expect(
    screen
      .getByRole("status")
      .element()
      .textContent?.match(/\[gap:/g),
  ).toHaveLength(1);
  expect(FakeSource.all.length).toBeLessThanOrEqual(5);
});

test("heartbeats keep a quiet stream live; 30 s of silence is a break: a gap, then a reopen (#123)", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeSource);
  const screen = await renderInFrame(<Probe path="/s" />);
  const first = FakeSource.all[0];
  if (!first) throw new Error("no source");
  first.onopen?.();
  first.emit("line", { text: "a" });
  for (let i = 0; i < 4; i++) {
    await vi.advanceTimersByTimeAsync(15_000);
    first.emit("heartbeat", { serverTime: "2026-10-29T12:00:00Z" });
  }
  expect(screen.getByRole("status").element().textContent).toBe("live||a");
  expect(first.closed).toBe(false);
  expect(FakeSource.all).toHaveLength(1);

  // A half-open stream: nothing more arrives.
  await vi.advanceTimersByTimeAsync(29_999);
  expect(first.closed).toBe(false);
  await vi.advanceTimersByTimeAsync(1);
  expect(first.closed).toBe(true);
  expect(screen.getByRole("status").element().textContent).toMatch(/^down\|\|a,\[gap:the stream was interrupted/);
  await vi.advanceTimersByTimeAsync(2_000);
  expect(FakeSource.all).toHaveLength(2);
});

test("a stream that opens but never delivers an event gives up like one that fails to open", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeSource);
  const screen = await renderInFrame(<Probe path="/s" />);
  for (let i = 0; i < 5; i++) {
    FakeSource.all[i]?.onopen?.();
    await vi.advanceTimersByTimeAsync(30_000 + 10_000);
  }
  expect(screen.getByRole("status").element().textContent).toContain("ended|the stream could not be reopened");
  expect(FakeSource.all).toHaveLength(5);
});
