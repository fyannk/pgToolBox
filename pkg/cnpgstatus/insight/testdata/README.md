# Insight pack fixtures

`pg18-primary.prom` and `pg14-primary.prom` are real exporter output:
CloudNativePG v1.30.0's own metrics package (its query runner, column
mappings and client_golang encoder, so label order and NaN rendering
are the exporter's) running CloudNativePG's default queries and then
the pack (`charts/pgtools/files/insight-pack/queries.yaml`) once against
a throwaway `postgres:18` or `postgres:14` container.

They are regenerated whenever a pack query changes. The parser tests
read them, so any change to a column, a label or the order of labels
shows up there.

## Regenerating

```sh
cd pkg/cnpgstatus/insight/testdata
generate/run.sh 18 pg18-primary.prom
NOLOCK=1 generate/run.sh 14 pg14-primary.prom
```

`run.sh` builds the generator (`generate/build.sh`), starts the
container, loads the scenario, runs the pack and removes the container.
It needs Docker and Go, and network access the first time, to download
the CloudNativePG module. With the catalog lock in place, CloudNativePG's
default queries (which have no lock_timeout) wait for the held sessions
to end, so a PostgreSQL 18 run takes about ten minutes.

`build.sh` does not vendor CloudNativePG code. It copies `collector.go`,
`mappings.go` and `parser.go` from the v1.30.0 module and cuts the
plugin collector, which imports internal packages. It then adds
`gen.go.txt`, which runs every query once against plain connections the
way `createMetricsFromUserQueries` does, and `main.go.txt`, which writes
the text exposition.

## The scenario (`generate/setup.sql`)

| What | Where | Expected |
| --- | --- | --- |
| Autovacuum threshold | `app.accounts`: 10,000 analyzed rows, scale 0.2, base 50 | 2,050 (the operator-precedence bug gave 502,000) |
| Threshold cap (18+) | `app.capped`: reloption `autovacuum_vacuum_max_threshold = 500` | 500 on 18, 2,050 on 14 |
| Never analyzed | `app.fresh` | reltuples and threshold unknown (NaN) |
| TOAST | `app.documents`, dead TOAST chunks | TOAST row names `public.documents` |
| Duplicates | `customers`, `orders`, `products`/`lines`, `rooms`, `vouchers`, `replid` | see `TestFixtureIndexes` |
| Not duplicates | other predicate, opclass, collation, `DESC`, deferrable, `NULLS NOT DISTINCT` (15+) | not reported |
| Unused and FK support | `orders_customer_idx` serves `orders.customer_id`'s foreign key | `fk_support` 1 |
| Partitioned | `events_at` on `events` | the partition's index only |
| Invalid | `broken_a`, a unique index built concurrently over duplicate values | `kind="invalid"` |
| Lock tree (`generate/sessions.sh`) | `billing-batch` idle in transaction blocks `checkout`, which blocks `migrate` | one tree rooted at billing-batch |
| Prepared transaction | `fixture-secret-gid` holds a row lock that `psql` waits on | a `prepared transaction` blocker with pid 0, and the gid never exported |
| Per-database failure (18) | an ACCESS EXCLUSIVE lock on `pg_constraint` in `billing` | `pgtoolbox_tables` and `pgtoolbox_vacuum_progress` fail on billing only |
| No CONNECT | database `many` | skipped by `target_databases: ["*"]` |
| Horizons | the logical slot `fixture_slot` | `slot_catalog_xmin_age` set, `slot_xmin_age` NaN |

## Scale

`generate/run.sh 18 out.prom scale` fills `many` with 20,000 tables
and 80,400 indexes; 400 of the indexes are duplicates. Grant CONNECT on
`many` to the `cnpg_metrics_exporter` role, then run
`generate/time-queries.py <queries.yaml> <container> many`. It prints
three warm runs of each query, in milliseconds, run the way the
exporter runs them. The pack's documentation records the results.
