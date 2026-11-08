import type { LogLine } from "./LogViewer";

const MESSAGES = [
  '{"level":"info","msg":"checkpoint starting: time"}',
  '{"level":"info","msg":"checkpoint complete: wrote 412 buffers (2.5%)"}',
  '{"level":"info","logger":"wal-archive","msg":"Archived WAL file","walName":"000000020000000A00000042"}',
  '{"level":"error","msg":"could not receive data from WAL stream: server closed the connection unexpectedly"}',
  '{"level":"info","msg":"record with incorrect prev-link at 0/A0000F8"}',
];

/** A deterministic log for stories and tests: n lines one second apart from the fixture clock. */
export function fixtureLog(n: number, from = 0): LogLine[] {
  const t0 = Date.parse("2026-10-29T11:50:00Z");
  return Array.from({ length: n }, (_, i) => {
    const seq = from + i;
    return {
      seq,
      time: new Date(t0 + seq * 1000).toISOString(),
      text: MESSAGES[seq % MESSAGES.length] ?? "",
    };
  });
}
