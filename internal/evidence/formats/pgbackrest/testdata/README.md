# pgBackRest fixtures

Real pgBackRest 2.60.0 output (PostgreSQL 17.11, Debian bookworm, PGDG
packages), written by `hack/evidence/pgbackrest-generator` and reduced to the
files the reader opens plus a listing of everything else.

## Test-only passphrase

The `encrypted` and `format6` repositories use the repository passphrase
`pgtoolbox-fixture-test-only-passphrase`. **TEST ONLY**: it protects nothing
but these fixtures and must never be used for a real repository.

## Layout

Per repository (`plain`, `encrypted`, `format6`):

- `backup/demo/backup.info{,.copy}`, `archive/demo/archive.info{,.copy}`;
- `backup/demo/<label>/backup.manifest{,.copy}` (not for `format6`);
- `archive/demo/17-1/????????.history`;
- `listing.tsv`: every object under `archive/` as `key<TAB>size<TAB>mtime`,
  sorted. WAL segments exist only here: the tests' reader fails on opening
  any key that has no content, which enforces the names-only WAL rule;
- `pgbackrest-info.json`: `pgbackrest info --output=json` of that repository,
  the oracle `TestInfoFilesMatchPgBackRestInfoOutput` compares against.

`pgbackrest-version.txt` is the generator's `pgbackrest version`.

`retention-before` and `retention-after` are one repository of a second
cluster (unencrypted, two full backups taken under `repo1-retention-full=9`)
reduced before and after a real `pgbackrest expire --repo1-retention-full=1`.
`TestRetentionFollowsARealExpire` evaluates both against that retention: the
first reads `RetentionNotApplied`, the second healthy. They come from a later
generator run than the other three; each repository stands alone.

`pgbackrest-2.60.0-many` is a stanza of 260 backups of a third cluster
(`pgbackrest-many-backups` mode of the generator, run by hand: about five
minutes, so not part of `make test-pgbackrest-fixtures`): a full, then
incrementals with a differential every tenth, each annotated so that
`backup.info` is 1.1 MiB, holds a line over 100 KiB and escaped and
non-ASCII characters. Only `backup/demo/backup.info.gz` (the `.copy` is
byte-identical; the generator checks it), `archive/demo/archive.info` and
`labels.txt` (the labels of pgBackRest's `info --output=json`, the oracle)
are kept. `TestManyRealBackupsVerify` reads it.

Damaged repositories (a WAL gap, a `.copy` that disagrees, a missing prior,
a format-6 header on an otherwise format-5 file) are made in the tests from
these files; nothing synthetic is committed here.

## Goldens

- `golden/<variant>.report.json`: the full `Analyze` report, byte for byte.
- `golden/shape.json`: the part of each report that does not depend on the
  run. It is what the regeneration check compares.

The shape **keeps**: states and reasons; the info pair used, its problems,
agreement, encryption and format; the database version, catalogue and
control versions and db-id; archive ids; the segment size and its source;
per backup its type, prior and references as symbolic names
(`<n>:<type>#<k>`, numbered by `backup-timestamp-stop`), chain, manifest and
recovery-path states, the stop reason, off-target, consistency, target,
traversed and frontier timelines, usability, the `cnpg-backup-name`
annotation, the starting timeline, pgBackRest version and options; per
archive id the WAL state, the timelines with ranges, the gap count and the
history, backup-history, partial, unknown and duplicate counts; the timeline
history edges; the window's oldest and newest backups (symbolic), end
timeline, usable count and assumptions; finding codes and severities; the
count of listed objects per class, segments excluded.

The shape **drops**: labels, timestamps, system ids, checksums, salts,
sizes, LSNs, WAL names and segment counts, all of which change between runs.

## Regenerating

```sh
make test-pgbackrest-fixtures   # regenerate in Docker, compare the shape
make pgbackrest-fixtures        # regenerate and replace these files
go test ./internal/evidence/formats/pgbackrest -update   # then review the goldens
```
