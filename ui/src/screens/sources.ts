import { m } from "../paraglide/messages.js";

export function sourceStateWord(state: string): string {
  switch (state) {
    case "live":
      return m.source_live();
    case "pending":
      return m.source_pending();
    case "stale":
      return m.source_stale();
    case "unavailable":
      return m.source_unavailable();
    case "forbidden":
      return m.source_forbidden();
    case "notInstalled":
      return m.source_not_installed();
    case "disabled":
      return m.source_disabled();
    default:
      return state;
  }
}
