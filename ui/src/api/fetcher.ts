// The one HTTP function every generated hook calls. It knows the console's
// error model (RFC 9457 problems, Huma's ErrorModel) and nothing about any
// resource.
//
//  - 401 → reauth: the session expired; the shell offers a fresh sign-in and
//    never retries silently.
//  - 403 → forbidden, with the role that would allow the request when the
//    server names one.
//  - anything else non-2xx → an ApiError carrying the request id for a report.

import type { ErrorModel } from "./generated/model";

/**
 * Huma's ErrorModel. A 403's detail names the role to ask for; the request id
 * comes from the X-Request-Id header. `loginUrl` is read when a server sends
 * one; the console's sign-in page is /auth/login otherwise.
 */
export type Problem = ErrorModel & { loginUrl?: string };

/** The console's sign-in page (internal/console/api: the session cookie is set by /auth/login). */
export const DEFAULT_LOGIN_URL = "/auth/login";

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | null;
  readonly requestId: string | null;
  constructor(status: number, problem: Problem | null, requestId: string | null) {
    super(problem?.title ?? `HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
    this.requestId = requestId;
  }
  get reauth(): boolean {
    return this.status === 401;
  }
  get forbidden(): boolean {
    return this.status === 403;
  }
  /** 503: the API server could not be asked; the request is worth retrying. */
  get unavailable(): boolean {
    return this.status === 503;
  }
  get loginUrl(): string {
    const url = this.problem?.loginUrl;
    // Same-origin paths only: a problem body never redirects the reader elsewhere.
    return url?.startsWith("/") && !url.startsWith("//") ? url : DEFAULT_LOGIN_URL;
  }
}

async function readProblem(res: Response): Promise<Problem | null> {
  const type = res.headers.get("content-type") ?? "";
  if (!type.includes("json")) return null;
  try {
    return (await res.json()) as Problem;
  } catch {
    return null;
  }
}

type ReauthListener = (loginUrl: string) => void;
const reauthListeners = new Set<ReauthListener>();

/** Notified on every 401 so the shell can show the sign-in prompt once. */
export function onReauth(listener: ReauthListener): () => void {
  reauthListeners.add(listener);
  return () => {
    reauthListeners.delete(listener);
  };
}

/**
 * The console's CSRF header (internal/console/auth.RequestHeader): every unsafe
 * request under /api/ must carry it, and a cross-site form or image cannot set
 * it. The server also checks Origin / Sec-Fetch-Site, which the browser sends.
 */
export const CSRF_HEADER = "X-PgToolBox-Request";

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

/** Whether a request with this method changes something and must carry CSRF_HEADER. */
function isUnsafe(method: string | undefined): boolean {
  return !SAFE_METHODS.has((method ?? "GET").toUpperCase());
}

export async function apiFetch<T>(url: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  if (isUnsafe(init.method)) headers.set(CSRF_HEADER, "1");
  const res = await fetch(url, { ...init, credentials: "same-origin", headers });
  if (res.ok) {
    if (res.status === 204) return undefined as T;
    // A few operations answer a document rather than JSON (the assistant's
    // prompt pack is text/markdown): the generated client types them string.
    if (!(res.headers.get("content-type") ?? "application/json").includes("json")) return (await res.text()) as T;
    return (await res.json()) as T;
  }
  const problem = await readProblem(res);
  const err = new ApiError(res.status, problem, res.headers.get("x-request-id"));
  if (err.reauth) for (const l of reauthListeners) l(err.loginUrl);
  throw err;
}
