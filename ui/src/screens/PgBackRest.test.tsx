// What the Backups screen shows for a pgBackRest Cluster: the
// type and chain of each backup as Go joined them, the agent's window and
// findings, and the Barman screen left as it was.
import { expect, test } from "vitest";
import { backups, backupsPgBackRest } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { BackupsView } from "./Backups";

test("pgBackRest: the repository backups carry their type and chain, newest first", async () => {
  const screen = await renderInFrame(<BackupsView data={backupsPgBackRest} />);
  const stanza = screen.container.querySelector("[data-pgbr-stanza]");
  expect(stanza?.getAttribute("data-pgbr-stanza")).toBe("billing");
  const types = [...(stanza?.querySelectorAll("table [data-pgbr-type]") ?? [])].map((e) =>
    e.getAttribute("data-pgbr-type"),
  );
  expect(types).toEqual(["incr", "incr", "diff", "full"]);
  const chains = [...(stanza?.querySelectorAll("table [data-pgbr-chain]") ?? [])].map((e) =>
    e.getAttribute("data-pgbr-chain"),
  );
  expect(new Set(chains)).toEqual(new Set(["20261022-030004F"]));
  await expect.element(screen.getByText("pgBackRest recorded it with an error")).toBeVisible();
  await expect.element(screen.getByText("The agent's recovery window")).toBeVisible();
});

test("pgBackRest: a Backup object shows the label it took", async () => {
  const screen = await renderInFrame(<BackupsView data={backupsPgBackRest} />);
  const objects = screen.getByRole("table", { name: "Backup objects" });
  await expect.element(objects.getByText("20261022-030004F_20261028-030003I")).toBeVisible();
});

test("pgBackRest: no window says so instead of drawing one", async () => {
  const repo = backupsPgBackRest.evidence.repository;
  if (!repo?.pgbackrest) throw new Error("fixture");
  const data = {
    ...backupsPgBackRest,
    evidence: {
      ...backupsPgBackRest.evidence,
      repository: { ...repo, pgbackrest: { ...repo.pgbackrest, recoveryWindow: undefined } },
    },
  };
  const screen = await renderInFrame(<BackupsView data={data} />);
  const none = screen.getByText(/no recovery window for this stanza/);
  await expect.element(none).toBeVisible();
  expect(none.element().getAttribute("data-health")).toBe("warning");
});

test("pgBackRest: the agent's retention comparison, or that none was made", async () => {
  const screen = await renderInFrame(<BackupsView data={backupsPgBackRest} />);
  const result = screen.container.querySelector("[data-pgbr-retention]");
  expect(result?.getAttribute("data-pgbr-retention")).toBe("healthy");
  await expect.element(screen.getByText("4 full backups")).toBeVisible();
  await expect.element(screen.getByText("1 full, 1 differential, 2 incremental")).toBeVisible();
  const repo = backupsPgBackRest.evidence.repository;
  if (!repo?.pgbackrest) throw new Error("fixture");
  const unset = {
    ...backupsPgBackRest,
    evidence: {
      ...backupsPgBackRest.evidence,
      repository: { ...repo, pgbackrest: { ...repo.pgbackrest, retention: undefined } },
    },
  };
  const none = await renderInFrame(<BackupsView data={unset} />);
  expect(none.container.querySelector("[data-pgbr-retention]")).toBeNull();
  await expect.element(none.getByText(/given no retention/)).toBeVisible();
});

test("Barman: no pgBackRest panel or label", async () => {
  const screen = await renderInFrame(<BackupsView data={backups} />);
  expect(screen.container.querySelector("[data-pgbr-stanza]")).toBeNull();
  expect(screen.container.querySelector("[data-pgbr-type]")).toBeNull();
});
