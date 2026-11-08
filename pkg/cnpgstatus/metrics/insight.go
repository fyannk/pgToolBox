// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

// The insight pack's charted series (pkg/cnpgstatus/insight). They are
// in the instance catalog like any other: a Cluster that has not opted
// into the pack reports none of their names, so the store allocates
// nothing for them and the metrics screen draws nothing. The pack's
// row-shaped families (per table, per statement, per backend) are not
// time series and are read by the insight screens instead; InsightNotes
// carries their prose so the two cannot drift from the definitions.

var insightGroup = GroupDef{
	Key: "insight", Title: "Insight pack",
	Blurb: "What the pgToolBox insight pack adds once the Cluster opts in: transaction id consumption, lock waits and sessions left idle in a transaction.",
}

var insightCharts = []SeriesDef{
	{
		Key: "insight-xid-rate", Title: "Transaction ids consumed", Unit: "xids/s", Group: "insight",
		Kind: Counter, Aggregate: Max, Scope: ScopePrimary, Names: []string{"cnpg_pgtoolbox_xid_next_xid"},
		Summary: "How fast the primary assigns transaction ids: the speed of the wraparound clock.",
		Note: Note{
			Means: "The rate of the next transaction id the primary will assign (pg_snapshot_xmax of the current snapshot, read without assigning one). Only writing transactions consume an id; read-only ones do not.",
			Why:   "Every database's transaction id age grows at exactly this rate until VACUUM freezes it. Divided into the distance left to autovacuum_freeze_max_age or to the stop limit, it is the time left.",
			Watch: "A step up with no matching rise in commits usually means many small writing transactions, such as a loop committing row by row or a queue table. A sustained high rate shortens every wraparound horizon in proportion, so the freeze work has to keep pace.",
		},
	},
	{
		Key: "insight-lock-waiting", Title: "Backends waiting on a lock", Unit: "backends", Group: "insight",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pgtoolbox_backends_lock_waiting"},
		Summary: "Backends whose wait event is a heavyweight lock held by another backend.",
		Note: Note{
			Means: "Backends in pg_stat_activity with wait_event_type Lock: each is queued behind a lock another backend holds or is queued for, such as a row, a table or a transaction id.",
			Why:   "A lock queue is invisible in throughput charts until it is long. This count is where it appears first, and the blocking tree on the insight screen shows who is at its head.",
			Watch: "Short spikes are normal under concurrent updates of the same rows. A plateau means a backend at the head of the queue is not finishing, often an idle-in-transaction session or a DDL statement waiting for an ACCESS EXCLUSIVE lock, which then blocks every reader behind it too.",
		},
	},
	{
		Key: "insight-idle-in-transaction", Title: "Idle in transaction", Unit: "backends", Group: "insight",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pgtoolbox_backends_idle_in_transaction"},
		Summary: "Sessions that opened a transaction and are not running anything in it.",
		Note: Note{
			Means: "Backends whose state is 'idle in transaction' or 'idle in transaction (aborted)': the client sent BEGIN, perhaps some statements, and is now doing something else without committing or rolling back.",
			Why:   "An open transaction holds its locks and its snapshot. The locks block DDL and conflicting writes; the snapshot stops VACUUM from removing any row version newer than it, in every table of the database.",
			Watch: "A few for a second at a time is an application between statements. Any that stay are a connection-pool or error-handling bug; idle_in_transaction_session_timeout ends them on the server side.",
		},
	},
	{
		Key: "insight-idle-in-transaction-age", Title: "Longest idle in transaction", Unit: "s", Group: "insight",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pgtoolbox_backends_idle_in_transaction_max_seconds"},
		Summary: "How long the oldest idle-in-transaction session has been idle.",
		Note: Note{
			Means: "Seconds since the oldest idle-in-transaction backend last changed state, which for an idle session is when its last statement finished.",
			Why:   "Its age is how long its locks have been held and how long VACUUM has been held back by its snapshot.",
			Watch: "Minutes cost bloat; hours cost a lot of it, plus lock queues behind any DDL. A value that only climbs is a session that will never come back on its own.",
		},
	},
	{
		Key: "insight-client-backends", Title: "Client connections", Unit: "connections", Group: "insight",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pgtoolbox_backends_client"},
		Summary: "Client backends, the connections max_connections counts.",
		Note: Note{
			Means: "Backends of type 'client backend': application, pooler and administrator connections. Unlike the backends count beside it, it leaves out WAL senders, autovacuum and background workers, which have their own slots.",
			Why:   "This is the number that reaches max_connections. Past max_connections minus the reserved connections, ordinary roles are refused.",
			Watch: "A climb towards the ceiling that tracks traffic needs a pooler or a larger ceiling; one that does not track traffic is a leak. The insight screen breaks it down by database, application and state.",
		},
	},
}

// InsightNotes explains the pack's row-shaped families, keyed by query
// name, in the catalog's style: what a row counts, why a DBA reads it,
// what an unusual reading implies.
var InsightNotes = map[string]Note{
	"pgtoolbox_database": {
		Means: "Per database, the age of datfrozenxid and datminmxid: the transactions and multixacts since the oldest unfrozen row anywhere in it. template0 is included: it accepts no connection and still ages.",
		Why:   "The oldest database decides when the whole instance reaches the wraparound limits; autovacuum must freeze it before then.",
		Watch: "An age near autovacuum_freeze_max_age is normal and resolves as anti-wraparound autovacuum runs. One that keeps climbing past it means freezing cannot complete: an old open transaction, a forgotten prepared transaction or an abandoned replication slot holds the xmin horizon back.",
	},
	"pgtoolbox_tables": {
		Means: "Per database, the tables with the oldest relfrozenxid and relminmxid and those with the most dead rows, with their autovacuum threshold and when they were last vacuumed and analyzed.",
		Why:   "Wraparound is resolved table by table, and bloat accumulates table by table: the database totals do not say which table is in the way.",
		Watch: "Dead rows above the threshold with no recent autovacuum mean the workers are busy elsewhere or are being cancelled by conflicting locks. A temporary table with a high age is never vacuumed by autovacuum: only the session that owns it can.",
	},
	"pgtoolbox_vacuum_progress": {
		Means: "VACUUM runs in progress, manual or autovacuum, with their phase, heap blocks scanned and vacuumed, and index cycles.",
		Why:   "It says whether the freeze or the cleanup the other panels call for is under way, and how far along.",
		Watch: "More than one index vacuum cycle means maintenance_work_mem (or autovacuum_work_mem) is too small for the dead rows, so each index is scanned several times. An anti-wraparound autovacuum does not give way to lock requests: DDL on that table will queue behind it.",
	},
	"pgtoolbox_blocking": {
		Means: "Backends waiting on a heavyweight lock and the backends they wait for, as pg_blocking_pids reports them, with how long each has waited and how old its transaction is. No statement text.",
		Why:   "The head of a lock queue is the one backend to act on; everyone behind it is a symptom.",
		Watch: "A root blocker idle in transaction is an application that forgot to commit. A root blocker running DDL waits itself for an ACCESS EXCLUSIVE lock, and blocks every reader behind it while it waits: lock_timeout on DDL prevents that queue.",
	},
	"pgtoolbox_connections": {
		Means: "Client connections grouped by database, application_name and state, the forty largest groups.",
		Why:   "When connections approach max_connections, this says who holds them.",
		Watch: "Many idle connections from one application are an oversized pool; many active ones are load or a slow query; idle in transaction is a bug.",
	},
	"pgtoolbox_statements": {
		Means: "pg_stat_statements on the primary: the statements with the most total execution time and those with the highest mean, identified by queryid, with calls, rows and shared buffer hits and reads. The text stays in the database.",
		Why:   "Total time says where the instance spends its effort; mean time says what each call costs a user.",
		Watch: "A statement whose mean time doubled since yesterday without more rows per call usually changed plan or lost an index. A low hit ratio (reads high against hits) means its data is not in shared buffers.",
	},
	"pgtoolbox_indexes": {
		Means: "Per database, indexes never scanned since the statistics reset (unique and constraint indexes excluded), invalid indexes, and indexes identical to another on the same table.",
		Why:   "Every index costs write amplification, WAL and vacuum time; an unused or duplicate one costs that for nothing.",
		Watch: "'Unused' counts scans on the primary only, since the date shown: an index serving only queries on replicas, or only a monthly report, reads as unused here. An invalid index is usually a failed CREATE INDEX CONCURRENTLY: it is maintained on every write and never used.",
	},
}
