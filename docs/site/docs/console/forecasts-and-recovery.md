---
sidebar_position: 10
title: Volume forecasts and the recovery window
---

# Volume forecasts and the recovery window

Two estimates sit next to the raw facts. The **storage screen** says when each
volume will fill. The **backups screen** draws the span a restore can target and
answers "can I restore to this time?". Each one states its basis and what it does
not prove.

## Volume forecasts (storage screen)

### Where the data comes from

The console trends a volume only when an instance reports it:

- `volumeUsages` in `/pg/status` (one `statfs` per volume, tablespaces included),
- or, failing that, the `cnpg_disk_*` metrics (data and WAL volumes only).

Upstream CloudNativePG reports neither. The panel then says so and draws nothing,
and the blind spot `storage.volumeUsageNotReported` stays on the screen. Builds
that report volumes (such as the autoGrow fork) fill the panel on their own. The
console detects the data, never the build.

A volume counts as full for PostgreSQL when *available* reaches one WAL segment
(16 MiB) or 0.05 % of the filesystem. Available is `statfs` `bavail`, so the
reserved blocks are excluded. The forecasts run toward:

- the current filesystem's size, always;
- and, when the volume's storage section declares `autoGrow`, its `maxSize`. The
  operator grows the PVC in steps up to `maxSize`, so that date is when the steps
  run out, not when the next one happens.

`autoGrow` is not in the upstream `Cluster` type, so the console reads it from a
second, raw view of each Cluster, stripped to its name and its `autoGrow` stanzas
(well under 1 KB per Cluster). The two views share one cache configuration but
are two watch streams of the same objects.

Inodes get a forecast of their own, toward the current inode count.

### The long trend

| Variable | Default | Bounds | Meaning |
|---|---|---|---|
| `VOLUME_TREND_INTERVAL` | 15 m | 1 m to 1 h, not shorter than `INSTANCE_STATUS_INTERVAL` | One sample per volume per slot: the first reading of the slot. |
| `VOLUME_TREND_RETENTION` | 30 days | 7 to 90 days | How long the samples are kept, about 2,880 per volume by default. |
| `VOLUME_TREND_MAX_SERIES` | 16384 | 16 to 100,000 | Volumes trended at once. Past the bound, new volumes are not trended: the trended ones keep their history, refusals are counted, and an affected cluster shows the blind spot `storage.volumeTrendsAtCapacity`. A full series of 30 days at 15 minutes is about 23 KB. |

Memory per volume is about 23 KB at the defaults, so the default bound costs at
most about 360 MiB; rings grow as samples arrive, so a younger or smaller fleet
holds less. A volume silent for a whole retention is forgotten. The three
variables multiply: the console refuses to start when
`VOLUME_TREND_MAX_SERIES × VOLUME_TREND_RETENTION / VOLUME_TREND_INTERVAL`
samples of 8 bytes would exceed 768 MiB. Twice the default retention or twice
the default volumes still fits.

Count this memory when you size a console for a large fleet: a console sized
from a load test that ran for hours has not filled its trend yet, and it grows
by up to 360 MiB over the first 30 days.

How one reading becomes a sample (`Store.Observe` in
`pkg/cnpgstatus/volumetrend/store.go`):

```mermaid
flowchart TB
  reading["a reading of one volume, from a sweep"] --> valid{"available within total, a time set?"}
  valid -->|no| drop["dropped"]
  valid -->|yes| known{"volume already trended?"}
  known -->|"no, and VOLUME_TREND_MAX_SERIES reached"| refused["refused, counted; blind spot on its Cluster"]
  known -->|"yes, or room left"| latest["refreshes the latest reading"]
  latest --> first{"first reading of its slot?"}
  first -->|no| done["nothing more: the slot has its sample"]
  first -->|yes| gap{"slots missed since the last sample?"}
  gap -->|"a whole retention or more"| reset["ring emptied, starts again"]
  gap -->|"some"| gaps["one empty sample per missed slot"]
  gap -->|none| sample["sample appended to the ring"]
  reset --> sample
  gaps --> sample
```

A slot is `VOLUME_TREND_INTERVAL` long. The ring holds
`VOLUME_TREND_RETENTION / VOLUME_TREND_INTERVAL` samples, the oldest overwritten
first. A sample is the space and the inodes consumed; the filesystem's size and
inode count are kept apart, as a short list of changes, which is how the fit
below finds resizes. A sweep that read nothing leaves an empty sample: a gap, never
a zero.

When `HISTORY_PATH` is set, the trend is snapshotted to `volume-trends.gob` beside
the history journal. A snapshot is written once per slot and again at shutdown, so
the trend survives restarts. A snapshot taken at another cadence is ignored, and
the trend starts empty.

### The forecast

Each forecast is computed on demand from the ring, and cached until a new sample
arrives (`Store.Forecasts`, `extrapolate` in `forecast.go`):

```mermaid
flowchart TB
  latest["latest reading: free space"] --> full{"free at or below the floor?"}
  full -->|yes| fullnow["full now"]
  full -->|no| window["last 7 days of samples"]
  window --> resize{"size changed inside the window?"}
  resize -->|"yes, enough readings since"| since["fit only the readings since the change"]
  resize -->|"yes, too few since"| shift["shift the earlier readings by the jump"]
  resize -->|no| hourly["hourly medians, gaps skipped"]
  since --> hourly
  shift --> hourly
  hourly --> enough{"24 hourly points over a day or more?"}
  enough -->|no| few["too few points"]
  enough -->|yes| fit["Theil–Sen rate; recent half if 1.5 times faster"]
  fit --> growing{"growth over the window above the minimum?"}
  growing -->|no| flat["not growing"]
  growing -->|yes| date["full at: free minus floor, divided by the rate"]
```

The floor is the [full threshold](#where-the-data-comes-from) for space and zero
for inodes. The same steps run toward `maxSize` with autoGrow, with the free space
plus what the steps may still add.

1. The samples of the last **7 days** are reduced to hourly medians, so a burst
   inside one hour weighs one point.
2. A **Theil–Sen** fit (the median of the pairwise slopes) gives the rate. A
   one-off drop, such as a `VACUUM FULL` or WAL recycled after a checkpoint,
   barely moves it.
3. The forecast refuses rather than guesses:
   - **too few points** below 24 hourly points or one day of span;
   - **not growing** when the fitted growth over the window is under
     max(1 MiB, 0.05 % of the capacity), or max(16, 0.05 %) for inodes.
4. **Growing faster than linear**: when the recent half of the window grows at
   least 1.5 times faster than the first half, the date uses the recent rate. Both
   rates are shown.
5. The interquartile slopes give the earliest–latest range and a confidence:
   **high** when the spread of the slopes is under half the rate, over at least
   72 hourly points and without acceleration; **medium** under one and a half
   times the rate; **low** otherwise, or when the lower quartile is not growing.
   The acceleration check needs 12 hourly points in each half of the window.
6. **Resizes**: growing a filesystem shifts the space counted as used by the
   reserved blocks it adds (5 % on ext4 by default), a step a fit would read as
   growth. When the size changed inside the window, the fit uses only the
   readings since the last change if they meet the minimums above; otherwise the
   readings before each change are shifted by the jump across it. The forecast
   says which.
7. **Inodes on XFS**: an inode total that changes inside the window (XFS
   allocates inodes on demand) leaves no inode forecast.
8. **Two targets with autoGrow**: the space forecast always runs toward the
   current filesystem. Free space at or below one WAL segment (16 MiB) or
   0.05 % of the filesystem is **full now**, autoGrow or not: an expansion that is
   refused, slow or outpaced leaves PostgreSQL writing there. With autoGrow and a
   readable `maxSize` above the current size, a second forecast runs toward
   `maxSize`.

### WAL growing while archiving fails

PostgreSQL keeps every WAL segment until it is archived. When archiving fails
(`ContinuousArchiving` failing, or the primary's live archiver failing with a
backlog), the console flags one of the primary's volumes: the WAL volume, or the
data volume when WAL shares it. Growth on that volume is a symptom: fix archiving
first.

### Diagnostics

| Check | Finds |
|---|---|
| `volume-full-forecast` | A volume full within 14 days (warning) or 3 days (high), in bytes or inodes, or full now (critical, autoGrow or not). With autoGrow, the date is the one toward `maxSize`. |
| `wal-growing-archiving-failing` | The primary's WAL volume growing while archiving fails (high; critical once full). |
| `volume-autogrow-stopped` | The fork operator's `VolumeAutoGrowStopped` (`MaxSizeReached`, `WALArchivingFailing`) or `VolumeExpansionRefused` condition. |

On a cluster whose instances report no volumes, the two forecast checks report a
source that is off, not a fault.

## Recovery window (backups screen)

### With repository evidence

The bar is drawn from the [evidence agent's](../evidence-agent.md) recovery paths,
toward the newest timeline a path could follow. A timeline seen in the archive
but not connected by valid history is reported, not used as the target, and an
off-target path (a backup that cannot reach the newest timeline, or a path
toward a sibling branch that a default restore does not follow) is neither the
target nor drawn. A sibling may have a lower or a higher number than the
timeline `latest` resolves to: what counts is the ancestry, not the number.
What an off-target path still reaches is listed under the bar as "restorable
only by naming a timeline", with the base backup and the `targetTLI` a restore
along it must give (`recovery_target_timeline: 'N'`); a restore check on such a
time, when no default restore covers it, carries that hint as a caveat.

A timeline warning from the agent (a history file no default restore reads is
malformed) hides no coverage: it is shown above the bar with the file name and
added to every answer's caveats, since a restore that names that timeline would
fail.

- **Covered** spans: each path covers from its base backup's end to the
  archive-receipt time of the last contiguous WAL segment. Only a path whose WAL
  reaches its backup's end segment (the consistency point) is drawn. Where paths
  overlap, each piece names the newest backup whose own path covers it: the backup
  CloudNativePG restores from when no `backupID` is given.
- **Holes** between spans, each with a kind:
  - **missing WAL**: a confirmed gap;
  - **candidate gap**: seen by one scan, not confirmed by a second;
  - **unknown**: the evidence cannot tell.
- **Markers**: the repository's base backups (completed, failed, incomplete) and
  the failed `Backup` objects.

"Structurally covered" is the strongest claim. The agent reads segment names, not
WAL bytes, and never runs a restore. The latest point is bounded by when the last
segment *reached* the repository. The newest transactions may still exist only on
the instance, especially while archiving fails.

Unless a [restore drill](restore-drills.md) of the Cluster passed in the last 90
days (the window of the `restore-drill-overdue` check), the bar, the evidence
counts and the restore check also say "not restore-tested".

When the newest edge is uncertain (the newest span is not covered, or the recent
end is unknown or not restorable), the panel suggests two queries to copy and run
on the primary yourself: `SELECT * FROM pg_stat_archiver;` shows what PostgreSQL
last archived and last failed to archive, and `SELECT pg_switch_wal();` forces
the current segment out to the archive. After that, refresh the evidence (or,
without an agent, check `pg_stat_archiver` again). The panel always shows them on
the reported basis, whose end is never a WAL frontier. The console is read-only,
so it runs neither query.

### Without repository evidence

The operator's or plugin's own window (`serverRecoveryWindow`, or the in-tree
recoverability points) is drawn hatched as **reported**. It is never drawn as
covered.

The end of that window is the **last base backup**
(`serverRecoveryWindow[].lastSuccessfulBackupTime`), not the end of the archived
WAL: the operator does not report how far archived WAL reaches beyond it. The bar
and the overview label it so, and the time from it to now is drawn **unknown**,
never covered and never "not covered".

What the console can add there is the archiving instance's own report on
`/pg/status` (`pg_stat_archiver.last_archived_time`, shown on the overview as
"Last archived WAL", with the age of the sweep that read it). The time after the
window is split at that instant, and each span says what the archiver supports:

| Archiving (overview) | Before the last archived WAL time | After it |
|---|---|---|
| healthy, keeping up, and the sweep fresh | "a restore to this time may well be possible" | its WAL "may still be only on the primary" |
| failing (`ContinuousArchiving` critical, or a failure with backlog) | its WAL "may be in the repository" | its WAL "is likely not in the repository" |
| behind: N segments waiting (`readyWalFiles` at the backlog threshold on consecutive sweeps), no failure | "N segments waiting": its WAL "may not be in the repository yet" | "N WAL segments are waiting": its WAL "is likely still only on the primary" |
| anything else (unknown, stale) | its WAL "may be in the repository" | its WAL "may still be only on the primary" |

The archiver is `status.currentPrimary` (the designated primary in a replica
cluster). When that instance was not read, archiving is unknown: no other
instance that reports itself primary stands in for it. A `last_archived_time`
later than the sweep that read it (an instance clock running ahead) is read as
the sweep's time.

**The backlog threshold** is 3 waiting segments, or one more than the archiver's
parallelism when that is larger: the in-tree `wal.maxParallel`, or the
barman-cloud plugin's `ObjectStore` `spec.configuration.wal.maxParallel` (read
with the `ObjectStore` the console already reads for the reported window). The
archiver is the one CloudNativePG uses: the first enabled plugin with
`isWALArchiver: true`, else the in-tree `barmanObjectStore`, else the first
backup plugin. A barman-cloud entry listed for backups only, beside a pgBackRest
plugin that archives WAL, does not lend its `maxParallel` to pgBackRest. A
serial archiver that keeps up shows zero or one `.ready` file (two on a switch
caught mid-copy), but a parallel one does not: the in-tree archiver and the
barman-cloud plugin upload up to `maxParallel` segments at once and leave the ones
uploaded ahead as `.ready` until PostgreSQL asks for them, and pgBackRest's
asynchronous `archive-push` does the same. A busy, healthy primary routinely
shows a handful. So one reading proves nothing, and a **backlog** counts only
when the sweep sees the count at or above the threshold on two consecutive
sweeps (a failed read is skipped). When the console cannot know the parallelism
(a pgBackRest plugin, or the barman-cloud plugin when its `ObjectStore` is not
readable) it needs three sweeps, over which the count never shrinks and ends
larger than it began. A backlog turns the WAL archiving contributor to a warning
(`ArchiveBacklog`), even while `ContinuousArchiving` is `True`: a hung or slow
`archive_command` fails nothing. A count at the threshold that the sweeps kept
are too few to judge (a console that just started, before its second or third
sweep) is `unknown` (`ArchiveBacklogUnconfirmed`), never `ok`, and the recovery
screens read it as a backlog until enough sweeps can tell.

With `ContinuousArchiving` `True` but the archiving instance not read, archiving
is `unknown` (`PrimaryUnread`), with the time the condition last changed: the
condition is written only when an `archive_command`'s outcome changes, so on its
own it does not say that WAL reaches the repository now. One missed read (a
timeout, a slow answer) does not flip it: the same pod's reading from the sweep
before stands, with its own time as provenance, so the bar does not flicker. A
second miss in a row, or a restarted pod (a new UID has no last reading), is
unread. A hibernated Cluster
reads `unknown` (`Hibernated`): no instance runs, and nothing confirms that every
segment written before hibernation reached the repository. A replica cluster's
archiver is its designated primary, a standby (`isPrimary` false) that archives
with `archive_mode=always`; CloudNativePG counts `.ready` files only on a primary
(`pkg/management/postgres/probes.go` v1.30.1), so its `/pg/status` carries no
`readyWalFiles`. The console records no sample for it (nor for any standby) and
reads archiving as `unknown` (`ArchiveBacklogNotReported`), never `ok`: a
backlog cannot be seen there. A standby's zeros, including those of a pod that
was a designated primary before a replica-cluster promotion, are never read as a
quiet sweep or as an archiver that caught up.
The diagnostics findings on the same count (`cnpg-wal-archive-backlog`,
`cnpg-instance-archive-backlog`) fire much later, at 32 segments, so a screen
with the finding never shows archiving as healthy. A stalled archiver shows up in
the count: with `archive_timeout` set, written WAL completes a segment within
it, and a segment the archiver does not take is a `.ready` file. On an idle
primary that writes nothing, a hung archiver can hold a steady count that the
growth rule (unknown parallelism) does not flag.

The last archived time is when PostgreSQL archived a segment, not the newest
commit in it, so even "may well be possible" is an `unknown` answer: an evidence
agent is what can place the time. When the archiver was not read, the span says
the time is not known. Failing archiving also adds a caveat that names the last
archived time. The sweep counts as stale after three sweep intervals, the same
rule as the "Instance self-reports" contributor.

### Can I restore to…?

`GET /api/v1/namespaces/{ns}/clusters/{name}/backups/restore-check?at=<RFC 3339>`
reads its answer off the same timeline (`restoreCheck` in
`internal/console/status/recovery.go`), in this order:

```mermaid
flowchart TB
  window{"a window known?"} -->|no| unknown["unknown"]
  window -->|yes| before{"before the earliest point?"}
  before -->|"yes, evidence cut at a bound"| unknown
  before -->|yes| notcovered["notCovered"]
  before -->|no| basis{"basis: reported only?"}
  basis -->|"yes, and not after the latest point"| reported["reported"]
  basis -->|"yes, after the latest point"| unknown
  basis -->|no| segment{"segment holding the time"}
  segment -->|covered| covered["covered: name the base backup"]
  segment -->|"missing WAL or candidate gap"| gap["gap"]
  segment -->|"unknown edge"| unknown
  segment -->|"none, later than now"| tail["the answer for now"]
  segment -->|none| unknown
```

The answer is one of:

- `covered`: name the base backup to restore from;
- `notCovered`: before the earliest point (a confirmed hole after the latest
  point is a `gap`);
- `gap`;
- `reported`: only the unverified window covers the time;
- `unknown`.

Every answer comes with its caveats. The uncertain edges are drawn as `unknown`
segments, so the bar and the answer always agree:

- the end of the archived WAL and the edge of a hole, within three
  `archive_timeout` periods (at least 15 minutes; one hour when `archive_timeout`
  is 0): segment names cannot place the target;
- the oldest restore point, within the agent's refresh interval plus the age of
  its last scan: a retention purge may have removed that backup since;
- after a WAL archiving failure an instance reported (`lastFailedWALTime`, the
  newest over every instance read, so a former primary's failure survives a
  failover) in the day before the latest point, the day before the failure: WAL
  that arrives after an outage carries older commits than its arrival time. The
  band is dropped once the archiver has caught up before the latest point: its
  `last_archived_time` is after its `last_failed_time` (PostgreSQL archives in
  order, so the failed segment is in the repository) and every sweep from one
  before the latest point to now saw fewer `.ready` files than the threshold.
  A transient error (an object store's 503, retried) then leaves no trace on the
  bar. While the archiver has not caught up, or caught up only after the newest
  segment reached the repository, the band stays;
- while a backlog persists on the primary (above), from the newest sweep, at or
  before the agent's scan, that read **no** `.ready` file on the primary since
  it became primary, less one `archive_timeout`, with the count and that time:
  nothing waited then, so every segment completed by then was in the repository
  when the agent read it, while segments archived since may have reached it
  late. A reading of one or two files proves nothing (a slow archiver can hold
  them for long), and a failed read is no reading, so a run of failed reads
  widens the band, never narrows it. When no sweep the console keeps
  (`RING_RETENTION`, one hour by default; a console that just started keeps
  none) read zero, the sweep period is not known, or `archive_timeout` is 0,
  the band is the day before the latest point. The window is then a warning,
  never healthy;
- while the primary's archiver is not read (archiving `PrimaryUnread`, or the
  Cluster hibernated): the same band, from the newest sweep that read it with
  nothing waiting, else the day before the latest point. Nothing says how long
  the newest archived WAL waited on the primary, so the window is a warning and
  every answer carries the caveat "the primary's archiver was not read". A
  hibernated Cluster's window is `unknown` instead: it writes no WAL and runs no
  archiver, so there is nothing to act on, but nothing confirms that the WAL
  written before it reached the repository; it stays `unknown`, not amber, for
  as long as it sleeps;
- while the archiver is a designated primary (`ArchiveBacklogNotReported`):
  the day before the latest point, with its own reason and caveat, since no
  sweep can show that nothing waited on it;
- a recovery path that reaches its frontier but that the evidence agent rates
  below healthy (a pgBackRest WAL segment size not proven across a log id or a
  timeline switch, or a path capped because the agent's coverage roll-up says
  something no path carries, such as a stanza split) is drawn as `unknown` with
  the agent's reason, never as covered; a healthy path still covers what it
  reaches. The window is then a warning. A roll-up that is not healthy because
  of another backup (an old backup stopped at a gap, one unknown path) is
  listed as a reason of the window and greys out nothing.

Inside the end-of-archived-WAL edge, the last `archive_timeout` plus an
allowance for the archiver's latency (one sweep interval, at least one minute)
before the latest point (the whole edge when `archive_timeout` is 0) is drawn
as **end of the archived WAL** (`walEnd`, a warning band): the last commit can
be one `archive_timeout` before its segment switched, and the segment reached
the repository some time after the switch. The latest
point is when the last segment *reached* the repository, not its last commit, so
the bar labels it **WAL received until**, and names apart the **newest covered
target** (`newestCoveredTarget`): the end of the newest covered span once every
edge above is drawn, the newest instant the check answers `covered`. It is
structurally covered, not restore-tested (only a passed restore drill tests a
restore), and it is left out when only a path not rated healthy reaches the
newest end (a healthy path that ends there too names it). A `covered`
answer adds that a restore that names no timeline follows the default
`recovery_target_timeline 'latest'`. A
`recovery_target_time` after that commit makes PostgreSQL stop with "recovery
ended before configured recovery target was reached". To restore everything the
repository holds, restore to the end of WAL (no recovery target) instead.

After the latest point the answer is never `notCovered`. Missing WAL confirmed
after the newest path is a `gap`. Otherwise the answer is `unknown`: a fresh and
complete frontier is a fact about the agent's scan, not about now. WAL written
just after the frontier may have sat in a segment that was not archived yet
when the agent read the repository (`archive_timeout`), and it may have arrived
since. The answer then says the time was "not in the repository when it was
last read" and names the scan time; while archiving fails it says that time's
WAL "is likely not in the repository", and with a backlog that it "is likely
still only on the primary". A time later than now gets the answer for
now. After a path that stops at unknown evidence, from stale evidence, when a
collection was cut, or on the reported basis (whose end is the last base backup),
it is also `unknown`.

On the reported basis, a window that comes from the evidence agent's own
`recovery_window` (pgBackRest without recovery paths) is named as the agent's,
not the operator's. Every sentence of the timeline, the answer and its caveats
carries a stable code and its values, so the console says it in the reader's
language, with Go's English kept for a code the interface does not know yet.
The timeline's reasons are sent as such phrases too, one per part. The evidence
agent writes its own reasons in English: the console recognises the agent's
timeline reasons ("timeline N needs an explicit targetTLI", "a default restore
stops on timeline N", a history file missing or unreadable) and says them in the
reader's language; any other reason of the agent's is shown as written. When the
evidence names a timeline history file as missing or unreadable (one such file
can withhold every restore point), the restore check names it, for example
`00000005.history`. Stale evidence is never shown as healthy. When reading
the paths fails, the last good ones are kept and marked stale.
