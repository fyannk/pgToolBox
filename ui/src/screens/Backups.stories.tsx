import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent, within } from "storybook/test";
import { backups, operationsOperator } from "../../mocks/fixtures";
import {
  evidenceAudienceMismatch,
  evidenceStatusNotFromOperator,
  recoveryAfterFailover,
  recoveryBackupUnconfirmed,
  recoveryHealthy,
  recoveryLiveTimelineNoSegment,
  recoveryMixedHoles,
  recoveryNone,
  recoveryReportedArchivingFailing,
  recoveryReportedOnly,
  recoveryRepositoryAhead,
} from "../../mocks/fixtures/extra";
import { handlersFor } from "../../mocks/handlers";
import type { OperationOffer, RestoreDrill } from "../api/generated/model";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { BackupsScreen, BackupsView } from "./Backups";

/** How the Cluster backs up, recency and archiving, every Backup and schedule, and the repository evidence cross-check (the agent's conclusions, never a restore promise). */
const meta = {
  title: "Screens/Backups",
  component: BackupsView,
  args: { data: backups },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof BackupsView>;
export default meta;
type Story = StoryObj<typeof meta>;

/** The golden: repository evidence with a hole where WAL is missing, and a failed base backup on the bar. */
export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Repository cross-check" })).toBeVisible();
    await expect(canvas.getByText(/strongest claim the agent makes/)).toBeVisible();
    await expect(canvas.getByRole("heading", { name: "Recovery window" })).toBeVisible();
    await expect(canvas.getByText("the window has holes where WAL is missing")).toBeVisible();
    const spans = canvas.getByRole("list", { name: "Spans of the recovery window" });
    await expect(spans).toHaveTextContent(/WAL missing/);
    await expect(canvas.getByText(/^Not verified: WAL continuity, judged from segment names only/)).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
/** No agent serves the repository: a capability not installed, not a fault. */
export const NoAgent: Story = {
  args: {
    data: { ...backups, evidence: { ...backups.evidence, state: "noAgent", health: "info", repository: undefined } },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/not installed, which is not a fault/)).toBeVisible();
  },
};
export const Empty: Story = { args: { data: { ...backups, items: [], schedules: [] } } };
const screen = { render: () => <BackupsScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
export const Forbidden: Story = { ...screen, parameters: { msw: handlersFor("forbidden") } };

/** The agent refused the console's token (audience mismatch); Go's reason replaces the generic word. */
export const EvidenceAudienceMismatch: Story = {
  args: { data: { ...backups, evidence: evidenceAudienceMismatch } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/not verifiable: audience mismatch/)).toBeVisible();
    await expect(canvas.queryByText(/did not answer or its answer was refused/)).toBeNull();
  },
};
export const EvidenceAudienceMismatchDark = dark(EvidenceAudienceMismatch);
/** The agent's status was not written by the operator; the console does not call it. */
export const EvidenceStatusNotFromOperator: Story = {
  args: { data: { ...backups, evidence: evidenceStatusNotFromOperator } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/not verifiable: status not written by the operator/)).toBeVisible();
  },
};
export const EvidenceStatusNotFromOperatorCompact = compact(EvidenceStatusNotFromOperator);

/** The repository covers the whole window: one covered span. */
export const RecoveryHealthy: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryHealthy } },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("list", { name: "Spans of the recovery window" })).toHaveTextContent(
      /structurally covered/,
    );
  },
};
export const RecoveryHealthyDark = dark(RecoveryHealthy);
export const RecoveryHealthyCompact = compact(RecoveryHealthy);
/** Right after a failover: a warning that the repository holds no WAL of the new timeline yet, saying what to do. */
export const RecoveryAfterFailover: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryAfterFailover } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/run pg_switch_wal\(\) on the new primary/)).toBeVisible();
    await expect(canvas.getByRole("list", { name: "Spans of the recovery window" })).toHaveTextContent(
      /no WAL of timeline 2/,
    );
  },
};
/** The same in French. */
export const RecoveryAfterFailoverFrench: Story = french({
  args: RecoveryAfterFailover.args,
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/lancez pg_switch_wal\(\) sur le nouveau primaire/)).toBeVisible();
  },
});
/** The failover's history file is archived but no WAL segment of the new timeline yet: the tail says so. */
export const RecoveryLiveTimelineNoSegment: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryLiveTimelineNoSegment } },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("list", { name: "Spans of the recovery window" })).toHaveTextContent(
      /history file of timeline 2.*no WAL segment of it yet/,
    );
  },
};
/** The same in French. */
export const RecoveryLiveTimelineNoSegmentFrench: Story = french({
  args: RecoveryLiveTimelineNoSegment.args,
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("list", { name: "Intervalles de la fenêtre de restauration" })).toHaveTextContent(
      /fichier d’historique de la timeline 2.*encore aucun segment WAL/,
    );
  },
});
/** Paths kept stale beside a cut backup list that does not list the oldest backup: its span is not covered. */
export const RecoveryBackupUnconfirmed: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryBackupUnconfirmed } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/base backup 20261022T070800 is not in the repository’s backup list/)).toBeVisible();
    await expect(canvas.getByRole("list", { name: "Spans of the recovery window" })).toHaveTextContent(
      /may have been purged since/,
    );
  },
};
/** The same in French. */
export const RecoveryBackupUnconfirmedFrench: Story = french({
  args: RecoveryBackupUnconfirmed.args,
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/la sauvegarde de base 20261022T070800 ne figure pas/)).toBeVisible();
  },
});
/** The repository's newest timeline is ahead of the live one: another cluster may archive here. */
export const RecoveryRepositoryAhead: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryRepositoryAhead } },
  play: async ({ canvas }) => {
    await expect(
      canvas.getByText(/ahead of timeline 2.*restore points past the fork are not counted as covered/),
    ).toBeVisible();
    await expect(canvas.getByRole("list", { name: "Spans of the recovery window" })).toHaveTextContent(
      /may restore that cluster’s data/,
    );
  },
};
/** The same in French. */
export const RecoveryRepositoryAheadFrench: Story = french({
  args: RecoveryRepositoryAhead.args,
  play: async ({ canvas }) => {
    await expect(
      canvas.getByText(/en avance sur la timeline 2.*au-delà de la bifurcation ne sont pas comptés/),
    ).toBeVisible();
  },
});
/** Every kind of hole, an incomplete base backup, and a collection cut at a bound: a lower bound. */
export const RecoveryMixedHoles: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryMixedHoles } },
  play: async ({ canvas }) => {
    const legend = canvas.getByRole("list", { name: "Legend" });
    await expect(legend).toHaveTextContent(/possible hole/);
    await expect(legend).toHaveTextContent(/base backup incomplete/);
    await expect(canvas.getByText(/^Lower bound:/)).toBeVisible();
  },
};
export const RecoveryMixedHolesDark = dark(RecoveryMixedHoles);
/** No evidence agent: the operator's window, hatched and labelled unverified; no lower-bound note. */
export const RecoveryReportedOnly: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryReportedOnly } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Reported by the operator, not verified against the repository")).toBeVisible();
    await expect(canvas.getByRole("list", { name: "Legend" })).toHaveTextContent(
      /reported by the operator, not verified/,
    );
    await expect(canvas.queryByText(/^Lower bound:/)).toBeNull();
  },
};
export const RecoveryReportedOnlyDarkCompact = darkCompact(RecoveryReportedOnly);
/**
 * No evidence agent and WAL archiving failing: after the last base backup
 * the time is unknown, and after the primary's last archived segment its
 * WAL is likely not in the repository. The archiver hint shows, without
 * "refresh the evidence" (there is no agent).
 */
export const RecoveryReportedArchivingFailing: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryReportedArchivingFailing } },
  play: async ({ canvas, canvasElement }) => {
    const spans = within(canvas.getByRole("list", { name: "Spans of the recovery window" }));
    await expect(spans.getByText(/is likely not in the repository\.$/)).toBeVisible();
    await expect(spans.queryByText(/may well be possible/)).toBeNull();
    await expect(canvasElement.querySelector('[data-pitr-edge-hint="reported"]')).not.toBeNull();
    await expect(canvas.getByText(/check pg_stat_archiver again:$/)).toBeVisible();
  },
};
/** The same in French: Go's sentences are said from their codes, not shown in English. */
export const RecoveryReportedArchivingFailingFrench: Story = french({
  args: RecoveryReportedArchivingFailing.args,
  play: async ({ canvas }) => {
    const spans = within(canvas.getByRole("list", { name: "Intervalles de la fenêtre de restauration" }));
    await expect(spans.getByText(/ne sont probablement pas dans le dépôt\.$/)).toBeVisible();
    await expect(spans.queryByText(/likely not in the repository/)).toBeNull();
  },
});
/** Neither evidence nor an operator window: no bar, Go's reason. */
export const RecoveryNone: Story = {
  args: { data: { ...backups, recoveryTimeline: recoveryNone } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No recovery window")).toBeVisible();
    await expect(canvas.queryByRole("img", { name: /Recovery window from/ })).toBeNull();
  },
};
/** "Can I restore to…?" through the API (MSW): the answer keeps "structurally covered" and lists its caveats. */
export const RestoreCheck: Story = {
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(canvas.getByLabelText("Restore target (UTC)")).toHaveValue("2026-10-29T11:59");
    await userEvent.click(canvas.getByRole("button", { name: "Check" }));
    await expect(await canvas.findByText(/: Structurally covered$/)).toBeVisible();
    await expect(canvas.getByText("What this answer does not prove:")).toBeVisible();
  },
};

// Restore drills: the latest result first, each drill with its
// checks, what a drill does not check, and the offer to run one.
const drills = backups.drills;
const [passed, failed] = drills.items as [RestoreDrill, RestoreDrill];
const drillOffer = (operationsOperator.offers ?? []).find((o) => o.kind === "restoreDrill") as OperationOffer;
const running: RestoreDrill = {
  ...passed,
  drill: { namespace: "team-a", name: "drill-orders-2610291200" },
  startedAt: "2026-10-29T12:00:00Z",
  deadline: "2026-10-29T14:00:00Z",
  healthyAt: undefined,
  finishedAt: undefined,
  tornDownAt: undefined,
  durationSeconds: undefined,
  restoredBytes: undefined,
  timeline: undefined,
  result: "running",
  health: "progressing",
  checks: (passed.checks ?? []).map((c) => ({ ...c, outcome: "pending" as const, text: "waiting" })),
};

/** Drills are off on this console: said once, nothing else. */
export const DrillsOff: Story = {
  args: { data: { ...backups, drills: { ...drills, enabled: false, items: [], lastPassedAt: undefined } } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Restore drills are off on this console.")).toBeVisible();
  },
};
/** On, never run: no success is claimed. */
export const DrillsNeverRun: Story = {
  args: { data: { ...backups, drills: { ...drills, items: [], lastPassedAt: undefined } } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/No successful restore drill is recorded/)).toBeVisible();
  },
};
/** A drill in progress: its checks are pending, its deadline is shown. */
export const DrillRunning: Story = {
  args: { data: { ...backups, drills: { ...drills, items: [running, passed, failed] } } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Drill drill-orders-2610291200 is running/)).toBeVisible();
    await expect(canvas.getAllByText("pending").length).toBeGreaterThan(0);
  },
};
export const DrillRunningDark = dark(DrillRunning);
/** Passed then failed on size: the reason is the headline of the failed one. */
export const DrillPassedAndFailed: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Last successful drill/)).toBeVisible();
    await expect(canvas.getByText(/^size: restored 120.0 GiB/)).toBeVisible();
    await expect(canvas.getByText(/A drill does not check/)).toBeVisible();
  },
};
export const DrillPassedAndFailedCompact = compact(DrillPassedAndFailed);
/** Timed out, and the console could not delete the drill: said, never hidden. */
export const DrillTimedOutNotDeleted: Story = {
  args: {
    data: {
      ...backups,
      drills: {
        ...drills,
        items: [
          {
            ...failed,
            result: "timedOut",
            health: "critical",
            reason: "not healthy within the time limit (2h)",
            tornDownAt: undefined,
            teardownError: "clusters.postgresql.cnpg.io is forbidden: the teardown Role is missing",
          },
        ],
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("timed out")).toBeVisible();
    await expect(canvas.getByText(/could not delete the drill Cluster/)).toBeVisible();
  },
};
/** The reader may run one: typed name, then the request is sent as them. */
export const DrillRequest: Story = {
  args: {
    drillOffer: { ...drillOffer, allowed: true },
    actor: "oidc:jane@corp.example",
    onRequest: fn(async () => ({})),
  },
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Run a restore drill/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(within(dialog).getByText(/orders-daily-20261029070000/)).toBeVisible();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await expect(args.onRequest).toHaveBeenCalledWith({
      kind: "restoreDrill",
      target: undefined,
      confirmation: "orders",
    });
  },
};
/** Not offered: the missing permission is named. */
export const DrillDenied: Story = {
  args: { drillOffer, actor: "oidc:jane@corp.example", onRequest: fn(async () => ({})) },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/create clusters in team-a/)).toBeVisible();
  },
};
/** Not possible now: the offer says exactly what is missing. */
export const DrillBlocked: Story = {
  args: {
    drillOffer: {
      ...drillOffer,
      allowed: true,
      blockers: [
        {
          reason: "NoCompletedBackup",
          text: "the Cluster has no completed Backup to restore",
          source: "Backup.status.phase",
        },
      ],
    },
    actor: "oidc:jane@corp.example",
    onRequest: fn(async () => ({})),
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/no completed Backup to restore/)).toBeVisible();
    await expect(canvas.getByRole("button", { name: /Run a restore drill/ })).toBeDisabled();
  },
};
