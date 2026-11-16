// A failed refresh keeps the last reading and says how old it is (issue #92):
// "could not be refreshed" alone does not tell 10 s from 10 min.
import { expect, test } from "vitest";
import { NOW_MS } from "../../mocks/fixtures/extra";
import { ApiError } from "../api/fetcher";
import { renderInFrame } from "../testing/render";
import { QueryState } from "./QueryState";

const failedRefresh = (error: unknown) => ({
  data: "the last reading",
  dataUpdatedAt: NOW_MS - 10 * 60_000,
  error,
  isPending: false as const,
  refetch: async () => ({}) as never,
});

test("a failed refresh shows the last reading and its age, absolute UTC on hover", async () => {
  const s = await renderInFrame(
    <QueryState query={failedRefresh(new Error("network"))} what="Events">
      {(d) => <p>{d}</p>}
    </QueryState>,
  );
  const status = s.getByRole("status");
  await expect
    .element(status)
    .toHaveTextContent("Events could not be refreshed; this is the last reading, as of 10 min. ago.");
  expect(status.element().getAttribute("title")).toContain("UTC");
  await expect.element(s.getByText("the last reading", { exact: true })).toBeVisible();
});

test("a 401 says nothing here: the re-authentication banner owns it", async () => {
  const reauth = new ApiError(401, { title: "Unauthorized", status: 401 }, "r");
  const s = await renderInFrame(
    <QueryState query={failedRefresh(reauth)} what="Events">
      {(d) => <p>{d}</p>}
    </QueryState>,
  );
  await expect.element(s.getByText("the last reading", { exact: true })).toBeVisible();
  expect(s.container.querySelector('[role="status"]')).toBeNull();
});
