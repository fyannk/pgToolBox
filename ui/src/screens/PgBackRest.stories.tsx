import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { backupsPgBackRest } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import type { Backups } from "../api/generated/model";
import { compact, dark, darkCompact } from "../testing/variants";
import { BackupsScreen, BackupsView } from "./Backups";

/**
 * The Backups screen of a Cluster backed up with a pgBackRest plugin.
 * Neither plugin reports recoverability on the Cluster: the stanza panel, its
 * recovery window and the backup chain come from the evidence agent, and the
 * Backup objects show the label they took. Use it to review both plugins'
 * shapes; the Barman screen is Screens/Backups.
 */
const meta = {
  title: "Screens/Backups/pgBackRest",
  component: BackupsView,
  args: { data: backupsPgBackRest },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof BackupsView>;
export default meta;
type Story = StoryObj<typeof meta>;

const pgbr = backupsPgBackRest.evidence.repository?.pgbackrest;
if (!pgbr) throw new Error("the pgBackRest fixture has no stanza evidence");

/** Opera's shape: an Archive object, a stanza named on the Cluster, the WAL in a split stanza (finding StanzaSplit). */
const opera: Backups = {
  ...backupsPgBackRest,
  method: {
    ...backupsPgBackRest.method,
    pluginName: "pgbackrest.cnpg.opera.com",
    pgbackrest: {
      plugin: "pgbackrest.cnpg.opera.com",
      flavor: "opera",
      display: "pgBackRest (Opera)",
      objectKind: "Archive",
      objectName: "billing-archive",
      stanza: "billing",
      archiveStanza: "billing-wal",
      repository: 1,
      encrypted: false,
    },
  },
  items: (backupsPgBackRest.items ?? []).map((b) => ({ ...b, pluginName: "pgbackrest.cnpg.opera.com" })),
  evidence: {
    ...backupsPgBackRest.evidence,
    repository: backupsPgBackRest.evidence.repository && {
      ...backupsPgBackRest.evidence.repository,
      pgbackrest: {
        ...pgbr,
        archiveStanza: "billing-wal",
        encrypted: false,
        findings: [
          {
            code: "StanzaSplit",
            severity: "warning",
            health: "warning",
            subject: "billing-wal",
            message: "backups and WAL are in different stanzas",
          },
        ],
      },
    },
  },
};

/** Dalibo: a Stanza object; full, differential and incremental backups with their chain; a failed incremental. */
export const Dalibo: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("pgBackRest (Dalibo)")).toBeVisible();
    await expect(canvas.getByRole("heading", { name: "pgBackRest stanza" })).toBeVisible();
    await expect(canvas.getByRole("heading", { name: "The agent's recovery window" })).toBeVisible();
    const table = within(canvas.getByRole("table", { name: "Backups in the stanza" }));
    await expect(table.getAllByText("Differential").length).toBeGreaterThan(0);
    await expect(table.getAllByText("Incremental").length).toBe(2);
    await expect(table.getByText("pgBackRest recorded it with an error")).toBeVisible();
    await expect(canvas.getByText("WALUnknown")).toBeVisible();
    await expect(canvas.getByText("4 full backups")).toBeVisible();
  },
};
export const DaliboDark = dark(Dalibo);
export const DaliboCompact = compact(Dalibo);
export const DaliboDarkCompact = darkCompact(Dalibo);

/** Opera: an Archive object, the stanza from the Cluster, a split WAL stanza and its finding. */
export const Opera: Story = {
  args: { data: opera },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("pgBackRest (Opera)")).toBeVisible();
    await expect(canvas.getByText("Archive object")).toBeVisible();
    await expect(canvas.getByText("StanzaSplit")).toBeVisible();
    await expect(canvas.getAllByText("billing-wal").length).toBeGreaterThan(0);
  },
};
export const OperaDark = dark(Opera);
export const OperaCompact = compact(Opera);

/** A stanza with no backup yet: no window, an empty chain table. */
export const EmptyStanza: Story = {
  args: {
    data: {
      ...backupsPgBackRest,
      items: [],
      evidence: {
        ...backupsPgBackRest.evidence,
        repository: backupsPgBackRest.evidence.repository && {
          ...backupsPgBackRest.evidence.repository,
          pgbackrest: { ...pgbr, recoveryWindow: undefined, findings: [], backups: [] },
        },
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/no recovery window for this stanza/)).toBeVisible();
    await expect(canvas.getByText("The stanza holds no backup.")).toBeVisible();
  },
};
export const EmptyStanzaDark = dark(EmptyStanza);

/** Expire is not applying the retention: the stanza still holds a full and WAL that repo-retention-full=1 expires. */
export const RetentionNotApplied: Story = {
  args: {
    data: {
      ...backupsPgBackRest,
      evidence: {
        ...backupsPgBackRest.evidence,
        repository: backupsPgBackRest.evidence.repository && {
          ...backupsPgBackRest.evidence.repository,
          pgbackrest: {
            ...pgbr,
            retention: pgbr.retention && {
              ...pgbr.retention,
              full: 1,
              effectiveArchive: 1,
              result: {
                state: "warning",
                health: "warning",
                code: "retention-warning",
                message: "the stanza holds backups or WAL the retention expires: expire is not applying it",
              },
            },
            findings: [
              {
                code: "RetentionNotApplied",
                severity: "warning",
                health: "warning",
                subject: "20261015-030002F",
                message: "the full backup is beyond repo-retention-full=1: expire would have removed it",
              },
            ],
          },
        },
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "The stanza against its retention" })).toBeVisible();
    await expect(canvas.getByText("retention-warning")).toBeVisible();
    await expect(canvas.getByText("RetentionNotApplied")).toBeVisible();
    await expect(canvas.getByText("1 full backups")).toBeVisible();
  },
};
export const RetentionNotAppliedDark = dark(RetentionNotApplied);

/** An agent given no retention: the comparison is not made, and the panel says so. */
export const NoRetention: Story = {
  args: {
    data: {
      ...backupsPgBackRest,
      evidence: {
        ...backupsPgBackRest.evidence,
        repository: backupsPgBackRest.evidence.repository && {
          ...backupsPgBackRest.evidence.repository,
          pgbackrest: { ...pgbr, retention: undefined },
        },
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/given no retention/)).toBeVisible();
  },
};

const screen = { render: () => <BackupsScreen namespace="team-a" cluster="billing" /> };
/** Through the API (MSW): billing's fixture. */
export const Live: Story = {
  ...screen,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("pgBackRest (Dalibo)")).toBeVisible();
  },
};
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
