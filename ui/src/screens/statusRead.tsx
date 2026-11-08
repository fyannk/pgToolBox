import { m } from "../paraglide/messages.js";

/** The words for an instance's /pg/status read outcome (viewmodel StatusReadState). */
export function statusReadWord(state: string): string {
  switch (state) {
    case "ok":
      return m.read_ok();
    case "pending":
      return m.read_pending();
    case "unreachable":
      return m.read_unreachable();
    case "notVerifiable":
      return m.read_not_verifiable();
    case "noTLS":
      return m.read_no_tls();
    case "tlsError":
      return m.read_tls_error();
    case "badResponse":
      return m.read_bad_response();
    case "notRunning":
      return m.read_not_running();
    case "disabled":
      return m.read_disabled();
    default:
      return state;
  }
}

/**
 * What an administrator can do about a read outcome that is a configuration
 * state rather than a fault. notVerifiable is honest: the console has no CA
 * for this Cluster and never reads it unverified.
 */
export function StatusReadHint({ state }: { state: string }) {
  if (state === "notVerifiable") return <span className="text-label text-muted">{m.read_not_verifiable_hint()}</span>;
  if (state === "noTLS") return <span className="text-label text-muted">{m.read_no_tls_hint()}</span>;
  return null;
}
