import { afterEach, expect, test, vi } from "vitest";
import { ApiError, apiFetch, CSRF_HEADER, DEFAULT_LOGIN_URL, onReauth } from "./fetcher";

afterEach(() => vi.restoreAllMocks());

function respond(status: number, body: unknown) {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/problem+json" } }),
  );
}

test("401 notifies the shell once with a same-origin login URL and throws", async () => {
  respond(401, { title: "Session expired", status: 401, loginUrl: "/auth/login?x=1" });
  const seen: string[] = [];
  const off = onReauth((u) => seen.push(u));
  await expect(apiFetch("/api/v1/clusters")).rejects.toBeInstanceOf(ApiError);
  off();
  expect(seen).toEqual(["/auth/login?x=1"]);
});

test("a login URL pointing elsewhere is never followed", async () => {
  respond(401, { title: "Session expired", status: 401, loginUrl: "https://evil.example/login" });
  const err = await apiFetch("/api/v1/clusters").catch((e: ApiError) => e);
  expect((err as ApiError).loginUrl).toBe(DEFAULT_LOGIN_URL);
  respond(401, { title: "x", status: 401, loginUrl: "//evil.example/login" });
  const err2 = await apiFetch("/api/v1/clusters").catch((e: ApiError) => e);
  expect((err2 as ApiError).loginUrl).toBe(DEFAULT_LOGIN_URL);
});

test("401 without a login URL falls back to the console's sign-in page", async () => {
  respond(401, { title: "Unauthorized", status: 401, detail: "sign in to use the console" });
  const err = (await apiFetch("/x").catch((e) => e)) as ApiError;
  expect(err.reauth).toBe(true);
  expect(err.loginUrl).toBe(DEFAULT_LOGIN_URL);
});

test("403 keeps the server's detail, which names the role to ask for", async () => {
  respond(403, { title: "Forbidden", status: 403, detail: "ask for the team-viewer role there" });
  const err = (await apiFetch("/x").catch((e) => e)) as ApiError;
  expect(err.forbidden).toBe(true);
  expect(err.problem?.detail).toContain("team-viewer");
});

test("the request id comes from the response header", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response("oops", { status: 502, headers: { "content-type": "text/plain", "x-request-id": "hdr-1" } }),
  );
  const err = (await apiFetch("/x").catch((e) => e)) as ApiError;
  expect(err.requestId).toBe("hdr-1");
  expect(err.problem).toBeNull();
});

test("a document answer (text/markdown) comes back as text, JSON as JSON", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
    new Response("# SKILL\n", { status: 200, headers: { "content-type": "text/markdown; charset=utf-8" } }),
  );
  await expect(apiFetch<string>("/api/v1/assistant-setup/skill")).resolves.toBe("# SKILL\n");
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
    new Response('{"a":1}', { status: 200, headers: { "content-type": "application/json" } }),
  );
  await expect(apiFetch("/api/v1/x")).resolves.toEqual({ a: 1 });
});

test("every unsafe request carries the CSRF header; safe ones do not", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(null, { status: 204 }));
  for (const method of ["POST", "PUT", "PATCH", "DELETE", "post"]) await apiFetch("/api/v1/x", { method });
  for (const method of [undefined, "GET", "HEAD"]) await apiFetch("/api/v1/x", { method });
  const sent = spy.mock.calls.map(([, init]) => new Headers(init?.headers).get(CSRF_HEADER));
  expect(sent).toEqual(["1", "1", "1", "1", "1", null, null, null]);
  for (const [, init] of spy.mock.calls) expect(init?.credentials).toBe("same-origin");
});

test("the caller's headers are kept beside the CSRF header", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(null, { status: 204 }));
  await apiFetch("/api/v1/x", { method: "POST", headers: { "Content-Type": "application/json" } });
  const h = new Headers(spy.mock.calls[0]?.[1]?.headers);
  expect(h.get("content-type")).toBe("application/json");
  expect(h.get("accept")).toBe("application/json");
  expect(h.get(CSRF_HEADER)).toBe("1");
});
