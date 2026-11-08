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

// Package metrics retains a bounded window of instance-reported metric
// samples so the console can show what the exporters claimed over time.
//
// Every value here is a claim by the instance's metrics endpoint, never
// a measurement the console made itself (AGENTS.md rule 8); the store
// records those claims verbatim and derives nothing beyond per-interval
// rates for counters and per-bucket aggregates for the rollup tier. A
// scrape that did not happen leaves a hole in the timestamps, and reads
// surface that hole as a gap rather than interpolating across it.
//
// The surface has two catalogs, because not every number wants a
// history. Catalog is the time series: values whose shape over time is
// the reading, each costing two ring buffers per instance. Instants is
// the point-in-time surface: a version string, a start time, a "1 if
// fenced" flag — numbers whose latest value is the whole story, kept as
// one sample each and drawn as status tiles. Splitting them is what
// lets the screen serve every metric the exporter offers without
// paying ring-buffer memory for the forty that never move.
//
// Both catalogs carry their own prose. A dashboard that shows
// cnpg_pg_stat_database_temp_bytes without saying what a rise in it
// means is a wall of lines; Note is that explanation, and it lives
// beside the definition so the two cannot drift.
//
// Bounds are structural, not policed: both catalogs are fixed at
// compile time, instances are capped, and both series tiers are ring
// buffers whose capacity follows from the configured interval and
// retention. Memory is therefore bounded by construction — there is
// nothing to evict and no cleanup loop to fall behind. At the defaults
// (10s interval, 6h raw, 7d of 5-minute rollups) one series costs one
// instance about 115 KiB, so the whole Catalog against the instance cap
// is the store's ceiling; an instrument is allocated only when its
// metric is actually reported, which is why a replica pays nothing for
// the primary-only archiver and pg_stat_replication series. Instants
// cost 24 bytes each.
package metrics

import "time"

// Kind says how a series' samples behave over time.
type Kind int

const (
	// Gauge samples are point-in-time values, stored as reported.
	Gauge Kind = iota
	// Counter samples are cumulative; the store converts them to
	// per-second rates at ingest, so a counter reset produces a gap
	// rather than a negative spike.
	Counter
)

// Aggregate says how label sets of one metric fold into one value.
type Aggregate int

const (
	// Sum adds all label sets, e.g. connections across databases.
	Sum Aggregate = iota
	// Max keeps the largest label set, e.g. the worst replication lag.
	Max
)

// Scope says which instances can report a metric at all, so the screen
// can say "only the primary reports this" instead of leaving a reader
// to wonder why two of three pods are empty.
type Scope int

const (
	// ScopeAll is reported by every instance.
	ScopeAll Scope = iota
	// ScopePrimary is reported only by the instance that is not in
	// recovery: pg_stat_archiver and pg_stat_replication are views on
	// work only a primary does.
	ScopePrimary
)

// Note explains one instrument in plain language. It is documentation,
// not a verdict: Watch says what a reading of a given shape would mean
// anywhere, never that this cluster is in that state — the console
// renders claims and does not diagnose (AGENTS.md rule 8).
type Note struct {
	// Means says what the number counts, in one sentence.
	Means string
	// Why says why an operator would look at it.
	Why string
	// Watch says what an unusual shape would imply, and what it would
	// cost if left alone.
	Watch string
}

// GroupDef names one section of the metrics screen.
type GroupDef struct {
	// Key is the stable identifier used in markup ids.
	Key string
	// Title is the section heading.
	Title string
	// Blurb introduces the section in one sentence.
	Blurb string
}

// instanceGroups are the instance screen's sections, in render order.
// Every catalog entry names one; an entry whose group is not listed
// here would never be rendered, which the catalog test holds against.
var instanceGroups = []GroupDef{
	{
		Key: "instance", Title: "Instance state",
		Blurb: "What the instance says it currently is: version, role, uptime, and the flags the operator sets on it.",
	},
	{
		Key: "sessions", Title: "Sessions and contention",
		Blurb: "Who is connected and whether anything is stuck waiting on someone else.",
	},
	{
		Key: "transactions", Title: "Transactions and rows",
		Blurb: "The work the database is actually doing, in transactions and in rows touched.",
	},
	{
		Key: "cache", Title: "Cache and disk I/O",
		Blurb: "Whether reads are being answered from shared buffers or from the disk underneath, and what queries spill.",
	},
	{
		Key: "background", Title: "Background writer and checkpoints",
		Blurb: "How dirty pages reach disk, and whether checkpoints are happening on schedule or under pressure.",
	},
	{
		Key: "wal", Title: "Write-ahead log",
		Blurb: "How much WAL the instance generates and how much of it is sitting on the data volume.",
	},
	{
		Key: "archive", Title: "WAL archiving and backup",
		Blurb: "Whether WAL is leaving the instance for the object store, and when the last backup landed.",
	},
	{
		Key: "replication", Title: "Replication",
		Blurb: "How far the replicas are behind, in time and in bytes, and how much WAL their slots are holding back.",
	},
	{
		Key: "storage", Title: "Storage and wraparound",
		Blurb: "What the databases occupy, and how close the transaction-id counters are to needing a freeze.",
	},
	{
		Key: "volumes", Title: "Volumes",
		Blurb: "The filesystems of the data and WAL volumes as the instance reads them with statfs, also while PostgreSQL is down. Reported by the cnpg_disk_* series of CloudNativePG builds that have them; empty elsewhere.",
	},
}

// SeriesDef is one curated time series. The catalog is the console's
// entire charted surface: nothing outside it is scraped or stored,
// which is what bounds cardinality.
type SeriesDef struct {
	// Key is the stable identifier used in URLs and storage.
	Key string
	// Title is the plain-language name.
	Title string
	// Unit labels the axis; rates are already per-second.
	Unit string
	// Group is the GroupDef key this series renders under.
	Group string
	// Summary is the one-line gloss shown under the title.
	Summary string
	// Note is the disclosed explanation.
	Note Note
	// Kind selects gauge or counter semantics.
	Kind Kind
	// Aggregate folds a metric's label sets into one value.
	Aggregate Aggregate
	// Scope says which instances report it at all.
	Scope Scope
	// Match, when set, keeps only the label sets whose labels all match;
	// it is how one metric name serves several series, as
	// cnpg_collector_pg_wal does with its value= label.
	Match map[string]string
	// Names are the candidate metric names, first match wins; a metric
	// absent from an exporter simply yields no sample.
	Names []string
}

// Catalog bundles one exporter's whole curated surface: the sections,
// the time series, and the point-in-time tiles. There is one per kind
// of thing scraped — the instances and the poolers run different
// software and answer different questions — and a Store carries the one
// it was built for, so neither can render the other's keys.
type Catalog struct {
	// Groups are the screen's sections, in render order.
	Groups []GroupDef
	// Series are the charted metrics.
	Series []SeriesDef
	// Instants are the point-in-time tiles.
	Instants []InstantDef
}

// SeriesByKey resolves a charted metric; ok is false for unknown keys.
func (c Catalog) SeriesByKey(key string) (SeriesDef, bool) {
	for _, def := range c.Series {
		if def.Key == key {
			return def, true
		}
	}
	return SeriesDef{}, false
}

// Instance is the CloudNativePG instance exporter's surface.
var Instance = Catalog{
	Groups:   append(append([]GroupDef(nil), instanceGroups...), insightGroup),
	Series:   append(append([]SeriesDef(nil), instanceCharts...), insightCharts...),
	Instants: instanceInstants,
}

// instanceSeries is the fixed time-series allowlist, chosen from the
// metrics the CloudNativePG instance exporter serves by default.
var instanceCharts = []SeriesDef{
	// --------------------------------------------------- sessions
	{
		Key: "connections", Title: "Connections", Unit: "connections", Group: "sessions",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_backends_total"},
		Summary: "Backends connected to this instance, across every database and state.",
		Note: Note{
			Means: "One count per backend process the instance is currently running, summed over every database, user and connection state the exporter reports — including the operator's own metrics-exporter session and the streaming-replication connections from the replicas.",
			Why:   "max_connections is a hard ceiling: when it is reached, new connections are refused outright, and the application sees connection errors rather than slow queries. Watching how close the count runs to the ceiling is the only warning you get.",
			Watch: "A step change with no deploy behind it usually means a pooler restarted or an application lost and re-made its pool. A slow climb that never falls back means connections are being leaked — something opens them and never closes them, and the instance will eventually refuse the next one.",
		},
	},
	{
		Key: "backends-waiting", Title: "Backends waiting", Unit: "backends", Group: "sessions",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_backends_waiting_total"},
		Summary: "Backends currently blocked waiting on another query.",
		Note: Note{
			Means: "The number of backends parked on a wait event caused by another backend — most often a lock one session holds and another wants.",
			Why:   "This is the single clearest 'something is stuck' number on the instance. Throughput can look normal while a queue forms behind one long transaction; this is where that queue shows up.",
			Watch: "Anything durably above zero is a contention problem, not a load problem: adding capacity will not clear it. A sustained rise usually traces back to one transaction holding a lock — often an idle-in-transaction session, which the maximum transaction duration beside this will confirm.",
		},
	},
	{
		Key: "max-tx-duration", Title: "Longest open transaction", Unit: "s", Group: "sessions",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_backends_max_tx_duration_seconds"},
		Summary: "Age of the oldest transaction still open on the instance.",
		Note: Note{
			Means: "The duration of the longest-running transaction, whether it is executing or sitting idle inside a BEGIN. The console keeps the largest value across label sets, so this is the worst offender, not an average.",
			Why:   "An open transaction pins the oldest transaction id the instance may consider dead. While it is open, autovacuum cannot reclaim any row version newer than it — anywhere in the database, not just in the tables that transaction touched.",
			Watch: "Minutes are normal for reporting queries; hours are a bug. A transaction left open for hours makes table bloat grow without bound, keeps xid age climbing toward wraparound, and blocks every DDL that needs a lock it holds. If this number only ever rises, an application is opening transactions it never commits.",
		},
	},
	{
		Key: "deadlocks", Title: "Deadlocks", Unit: "deadlocks/s", Group: "sessions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_deadlocks_total", "cnpg_pg_stat_database_deadlocks"},
		Summary: "Rate at which PostgreSQL is breaking deadlocks by killing a transaction.",
		Note: Note{
			Means: "How often the deadlock detector found two or more transactions waiting on each other and aborted one of them to break the cycle. Summed across databases and shown as a rate.",
			Why:   "Every deadlock is a transaction the application lost. PostgreSQL resolves the cycle correctly, but the losing session gets an error, and whether that error is retried or surfaces to a user is an application decision.",
			Watch: "This line is normally flat at zero, which is what makes any spike worth reading. Deadlocks come from two code paths taking the same locks in different orders; they cluster around a deploy, and they get worse under load because the window for the cycle widens.",
		},
	},
	{
		Key: "conflicts", Title: "Recovery conflicts", Unit: "cancellations/s", Group: "sessions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_conflicts_total", "cnpg_pg_stat_database_conflicts"},
		Summary: "Queries cancelled on a replica because replay needed to proceed.",
		Note: Note{
			Means: "Queries the instance killed because applying the primary's WAL would have removed rows the query was still reading. Only replicas can report this; a primary's line stays at zero.",
			Why:   "It is the visible cost of the replay-versus-read trade-off. A replica must either delay replay (and fall behind) or cancel the reader — this counts the times it chose to cancel.",
			Watch: "Rising conflicts mean read traffic on the replica is colliding with the primary's write rate. The usual levers are hot_standby_feedback (which moves the cost onto the primary as bloat) or max_standby_streaming_delay (which moves it onto replication lag). Neither is free; this number is how you tell which one you are currently paying.",
		},
	},

	// ----------------------------------------------- transactions
	{
		Key: "xact-commit", Title: "Transactions committed", Unit: "tx/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_xact_commit_total", "cnpg_pg_stat_database_xact_commit"},
		Summary: "Commit rate across every database on this instance.",
		Note: Note{
			Means: "Transactions that ended in COMMIT, per second. Every statement outside an explicit BEGIN is its own transaction, so a read-only workload commits too.",
			Why:   "This is the instance's throughput in the unit the database itself counts. Compared against the replicas' lines it also shows where write traffic actually lands.",
			Watch: "A drop to zero on the primary while connections stay up means the workload stopped, not the database. A drop with connections climbing means the opposite: work is arriving and not completing, which points at locks or at saturated I/O.",
		},
	},
	{
		Key: "xact-rollback", Title: "Transactions rolled back", Unit: "tx/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_xact_rollback_total", "cnpg_pg_stat_database_xact_rollback"},
		Summary: "Rollback rate — explicit ROLLBACK plus every failed statement.",
		Note: Note{
			Means: "Transactions that ended without committing, per second: application rollbacks, but also every transaction aborted by an error, a deadlock, a statement timeout or a cancelled query.",
			Why:   "Read against the commit rate it is an error rate. Applications rarely roll back on purpose in steady state, so a rollback ratio is usually a failure ratio.",
			Watch: "A rollback rate that tracks the commit rate proportionally is normal for a retry-heavy workload. A rollback rate that rises while commits fall is a failing deploy, an exhausted connection pool, or a lock timeout firing repeatedly.",
		},
	},
	{
		Key: "rows-returned", Title: "Rows returned by scans", Unit: "rows/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_tup_returned_total", "cnpg_pg_stat_database_tup_returned"},
		Summary: "Rows read off disk or cache by scans, before filtering.",
		Note: Note{
			Means: "Rows the executor examined — every row a sequential scan walked past, plus index entries traversed. It counts work done, not results produced.",
			Why:   "Paired with the fetched line below it is the cheapest sequential-scan detector there is. A query that reads a million rows to return ten shows up here and nowhere else.",
			Watch: "Returned running orders of magnitude above fetched means scans are reading far more than the queries need — a missing index, a query the planner cannot use one for, or a table that outgrew its plan. The instance will look I/O-bound and CPU-bound at once, and adding hardware only buys time.",
		},
	},
	{
		Key: "rows-fetched", Title: "Rows fetched by queries", Unit: "rows/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_tup_fetched_total", "cnpg_pg_stat_database_tup_fetched"},
		Summary: "Rows that actually satisfied a query, after filtering.",
		Note: Note{
			Means: "Rows the executor kept — the ones that matched and were handed onward. This is the useful half of the work the returned line counts.",
			Why:   "It is the denominator for the ratio above, and on its own it is the closest thing to a 'read throughput' figure the database reports.",
			Watch: "Fetched holding steady while returned climbs is plan degradation: the same answers are costing more work than they did. Both falling together is simply less traffic.",
		},
	},
	{
		Key: "rows-inserted", Title: "Rows inserted", Unit: "rows/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_tup_inserted_total", "cnpg_pg_stat_database_tup_inserted"},
		Summary: "Rows written by INSERT, per second.",
		Note: Note{
			Means: "New row versions created by INSERT. COPY counts here too, which is why a bulk load shows as a spike rather than a step.",
			Why:   "Insert rate drives table growth, index growth and WAL volume in the most direct way of any number on this screen.",
			Watch: "A sustained insert rate with no matching delete rate means the table only grows; whatever that table is, its indexes and its vacuum cost grow with it. Check it against database size in the storage section.",
		},
	},
	{
		Key: "rows-updated", Title: "Rows updated", Unit: "rows/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_tup_updated_total", "cnpg_pg_stat_database_tup_updated"},
		Summary: "Rows written by UPDATE, per second.",
		Note: Note{
			Means: "Row versions superseded by UPDATE. In PostgreSQL an update writes a new version and leaves the old one to be vacuumed, so this counts dead rows created as much as rows changed.",
			Why:   "Updates are the main source of bloat and of vacuum work. A workload that updates the same rows repeatedly costs far more than its row count suggests.",
			Watch: "A high update rate on a small table is the classic bloat generator: the table's physical size grows while its row count does not. If autovacuum cannot keep up — and the longest-open-transaction line above is what usually stops it — reads get slower for reasons no query plan explains.",
		},
	},
	{
		Key: "rows-deleted", Title: "Rows deleted", Unit: "rows/s", Group: "transactions",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_tup_deleted_total", "cnpg_pg_stat_database_tup_deleted"},
		Summary: "Rows removed by DELETE, per second.",
		Note: Note{
			Means: "Row versions marked dead by DELETE. The space is not returned to the operating system; it is returned to the table for reuse once vacuum has been through.",
			Why:   "Large deletes create large vacuum backlogs. The disk does not shrink when rows go away, which surprises people during cleanups.",
			Watch: "A big delete spike followed by database size not falling is expected, not a fault — the space is free inside the table. It only returns to the volume after a VACUUM FULL or a table rewrite, both of which take an exclusive lock.",
		},
	},

	// ----------------------------------------------------- cache
	{
		Key: "blocks-hit", Title: "Buffer cache hits", Unit: "blocks/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_blks_hit_total", "cnpg_pg_stat_database_blks_hit"},
		Summary: "8 KiB blocks answered from shared_buffers without touching disk.",
		Note: Note{
			Means: "Block reads satisfied by PostgreSQL's own shared buffer cache. It counts only PostgreSQL's cache — a block served by the operating system's page cache is counted as a read, not a hit.",
			Why:   "Against the read line below it gives the cache hit ratio, the oldest and still most useful single indicator of whether the working set fits in memory.",
			Watch: "A hit ratio that falls while traffic is flat means the working set outgrew shared_buffers, or a large scan evicted it. The instance does not fail; it just starts doing physical I/O for work it used to do in RAM, and every latency figure follows.",
		},
	},
	{
		Key: "blocks-read", Title: "Blocks read from disk", Unit: "blocks/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_blks_read_total", "cnpg_pg_stat_database_blks_read"},
		Summary: "8 KiB blocks PostgreSQL had to request from the storage layer.",
		Note: Note{
			Means: "Blocks not found in shared_buffers. Some of these were served by the operating system's page cache and never reached the disk — PostgreSQL cannot tell the difference, and neither can this line.",
			Why:   "It is the physical read demand the volume underneath has to absorb. Multiply by 8 KiB for a rough bytes-per-second figure to compare against the disk's rated throughput.",
			Watch: "A step up after a restart or a failover is the cache refilling and is expected. A sustained rise otherwise means either the data grew past memory or a new query is scanning something large.",
		},
	},
	{
		Key: "blk-read-time", Title: "Time spent reading blocks", Unit: "ms/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_blk_read_time_total", "cnpg_pg_stat_database_blk_read_time"},
		Summary: "Milliseconds per second backends spend blocked on reads.",
		Note: Note{
			Means: "Wall time backends spent inside read calls, in milliseconds per second of real time. Reported only when track_io_timing is on; with it off this line stays flat at zero and means nothing.",
			Why:   "Blocks read tells you how much I/O there is; this tells you how much it hurt. Ten thousand fast reads and a thousand slow ones are very different problems.",
			Watch: "Read time climbing faster than the block count means the storage got slower, not busier — a noisy neighbour on the volume, a throttled cloud disk, or a failing device. Values above 1000 ms/s mean more than one backend is blocked on I/O at all times.",
		},
	},
	{
		Key: "blk-write-time", Title: "Time spent writing blocks", Unit: "ms/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_blk_write_time_total", "cnpg_pg_stat_database_blk_write_time"},
		Summary: "Milliseconds per second backends spend blocked on writes.",
		Note: Note{
			Means: "Wall time backends spent writing data blocks themselves — which happens when they need a buffer and none is clean. Also needs track_io_timing.",
			Why:   "A backend writing its own dirty buffer is doing the background writer's job in the foreground, and the query waits while it happens.",
			Watch: "Any sustained value here says the background writer and the checkpointer are not keeping ahead of the workload. The section below is where the cause usually is: checkpoints happening on request rather than on schedule.",
		},
	},
	{
		Key: "temp-bytes", Title: "Temporary file bytes", Unit: "bytes/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_temp_bytes_total", "cnpg_pg_stat_database_temp_bytes"},
		Summary: "Bytes per second queries are spilling to temporary files.",
		Note: Note{
			Means: "Data written to temporary files because a sort, hash or materialisation did not fit in the memory the query was allowed.",
			Why:   "It is a direct, unambiguous work_mem signal. Nothing else on this screen tells you a query ran out of memory and went to disk instead.",
			Watch: "Any non-zero value is a query doing on disk what it wanted to do in RAM, at roughly a hundred times the cost. Sustained spilling also competes for the same volume the data is on, so it slows down queries that were not spilling. The fix is either more work_mem or a better plan — raising work_mem multiplies by the number of concurrent sorts, so it is not free.",
		},
	},
	{
		Key: "temp-files", Title: "Temporary files created", Unit: "files/s", Group: "cache",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_database_temp_files_total", "cnpg_pg_stat_database_temp_files"},
		Summary: "Rate at which queries are opening temporary files.",
		Note: Note{
			Means: "The count of temporary files created, as opposed to the bytes written into them.",
			Why:   "Bytes and files together separate 'one huge sort' from 'thousands of small spills'. They need different fixes.",
			Watch: "A high file rate with low byte volume means many queries are each spilling a little — usually one query shape running very often, just over the work_mem line. That one is cheap to fix and worth finding.",
		},
	},

	// ------------------------------------------------ background
	{
		Key: "buffers-alloc", Title: "Buffers allocated", Unit: "buffers/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_bgwriter_buffers_alloc_total", "cnpg_pg_stat_bgwriter_buffers_alloc"},
		Summary: "Rate at which backends are claiming shared buffers.",
		Note: Note{
			Means: "Buffer allocations — every time a backend needed a page and took a buffer for it, evicting whatever was there if necessary.",
			Why:   "It is the churn rate of shared_buffers. High allocation means the cache is being recycled constantly rather than holding a working set.",
			Watch: "High and rising allocation alongside a falling cache hit ratio is the signature of a working set that no longer fits. Sudden spikes are large scans sweeping the cache.",
		},
	},
	{
		Key: "buffers-clean", Title: "Buffers written by background writer", Unit: "buffers/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_bgwriter_buffers_clean_total", "cnpg_pg_stat_bgwriter_buffers_clean"},
		Summary: "Dirty buffers the background writer flushed ahead of demand.",
		Note: Note{
			Means: "Buffers written out by the background writer process, whose whole job is to have clean buffers ready before a backend asks for one.",
			Why:   "Work done here is work a query does not have to do inline. It is the cheap path for getting dirty pages to disk.",
			Watch: "Near-zero background writer activity while backend write time is high means the writer is not keeping up and backends are flushing their own buffers. bgwriter_lru_maxpages and bgwriter_delay are the knobs; the checkpointer's behaviour below is the other half of the picture.",
		},
	},
	{
		Key: "maxwritten-clean", Title: "Background writer stopped early", Unit: "stops/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_bgwriter_maxwritten_clean_total", "cnpg_pg_stat_bgwriter_maxwritten_clean"},
		Summary: "Times the background writer hit its per-round write limit and gave up.",
		Note: Note{
			Means: "Rounds in which the background writer stopped scanning because it had already written bgwriter_lru_maxpages buffers.",
			Why:   "It is the writer explicitly telling you it was configured to do less work than the workload needed. Few metrics are this unambiguous.",
			Watch: "Anything sustained above zero means bgwriter_lru_maxpages is the binding constraint: the writer wanted to keep flushing and was not allowed to. The work does not disappear — it moves to backends, and shows up as backend write time.",
		},
	},
	{
		Key: "checkpoints-timed", Title: "Scheduled checkpoints", Unit: "checkpoints/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_checkpointer_checkpoints_timed_total", "cnpg_pg_stat_checkpointer_checkpoints_timed"},
		Summary: "Checkpoints that happened because checkpoint_timeout came round.",
		Note: Note{
			Means: "Checkpoints triggered by the clock. These are the well-behaved ones: their writes are spread across checkpoint_completion_target, so the I/O is smooth.",
			Why:   "The ratio of timed to requested checkpoints is one of the most actionable tuning signals PostgreSQL exposes.",
			Watch: "You want this line to be almost all of your checkpoint traffic. Given the default 5-minute timeout, a healthy instance sits near 0.0033 checkpoints per second and stays there.",
		},
	},
	{
		Key: "checkpoints-req", Title: "Requested checkpoints", Unit: "checkpoints/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_checkpointer_checkpoints_req_total", "cnpg_pg_stat_checkpointer_checkpoints_req"},
		Summary: "Checkpoints forced by WAL volume rather than by the clock.",
		Note: Note{
			Means: "Checkpoints triggered because max_wal_size was reached before checkpoint_timeout expired — the instance ran out of WAL budget and had to checkpoint now.",
			Why:   "A requested checkpoint does not get to spread its writes. It is an I/O spike, and it happens under exactly the load that caused it.",
			Watch: "Requested checkpoints outnumbering scheduled ones means max_wal_size is too small for the write rate. The cost is latency spikes at each one, plus more full-page images in the WAL — which generates more WAL, which triggers the next one sooner. Raising max_wal_size costs disk and lengthens recovery; it is usually still the right trade.",
		},
	},
	{
		Key: "checkpoint-buffers", Title: "Buffers written by checkpoints", Unit: "buffers/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_checkpointer_buffers_written_total", "cnpg_pg_stat_checkpointer_buffers_written"},
		Summary: "Dirty buffers flushed by the checkpointer, including restartpoints.",
		Note: Note{
			Means: "Buffers written during checkpoints on a primary, or during restartpoints on a replica.",
			Why:   "It is the volume half of checkpoint cost, where the timed-versus-requested split is the shape half.",
			Watch: "Large sawtooth peaks lining up with requested checkpoints is the pattern to look for: each peak is an I/O burst the storage has to absorb while queries are running.",
		},
	},
	{
		Key: "checkpoint-write-time", Title: "Checkpoint write time", Unit: "ms/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_checkpointer_write_time_total", "cnpg_pg_stat_checkpointer_write_time"},
		Summary: "Milliseconds per second the checkpointer spends writing pages out.",
		Note: Note{
			Means: "Time the checkpointer spent in the write phase, which is deliberately spread out across checkpoint_completion_target.",
			Why:   "High write time is usually fine — spreading the writes is the point. It is the sync phase beside it that hurts.",
			Watch: "Read this together with sync time. Write time high and sync time low is a checkpointer doing its job gently; both high is storage that cannot keep up.",
		},
	},
	{
		Key: "checkpoint-sync-time", Title: "Checkpoint sync time", Unit: "ms/s", Group: "background",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pg_stat_checkpointer_sync_time_total", "cnpg_pg_stat_checkpointer_sync_time"},
		Summary: "Milliseconds per second the checkpointer spends in fsync.",
		Note: Note{
			Means: "Time spent forcing written pages down to durable storage at the end of a checkpoint. Unlike the write phase, this cannot be spread out.",
			Why:   "fsync stalls are the mechanism behind the checkpoint latency spikes users actually notice. Everything waits.",
			Watch: "Sync time rising while the buffer count stays flat means the storage layer is getting slower at durability, not busier — an overloaded volume, a filling cloud burst-credit balance, or a device degrading. This is one of the few numbers here that points squarely at hardware.",
		},
	},

	// ------------------------------------------------------- wal
	{
		Key: "wal-bytes", Title: "WAL generated", Unit: "bytes/s", Group: "wal",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_collector_wal_bytes"},
		Summary: "Bytes of write-ahead log the instance is producing.",
		Note: Note{
			Means: "WAL volume from pg_stat_wal, as a rate. Available on PostgreSQL 14 and later; on older instances this series simply has no samples.",
			Why:   "WAL rate sets the floor for three separate costs at once: what archiving has to ship, what replication has to stream, and how often max_wal_size forces a checkpoint.",
			Watch: "A rise with no matching rise in rows written usually means full-page images, not more data — see the FPI line beside this. Sustained WAL rate above what the object store can absorb makes the archive backlog grow without bound, and that ends with the data volume filling.",
		},
	},
	{
		Key: "wal-records", Title: "WAL records", Unit: "records/s", Group: "wal",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_collector_wal_records"},
		Summary: "Individual WAL records written per second.",
		Note: Note{
			Means: "The count of WAL records, as opposed to their size. Each is one logical change: a row version, an index entry, a commit.",
			Why:   "Bytes divided by records gives the average record size, which is how you tell a workload change from a full-page-image storm.",
			Watch: "Records flat while bytes climb means the records got bigger — almost always full-page images after a checkpoint. Records climbing with bytes is simply more write traffic.",
		},
	},
	{
		Key: "wal-fpi", Title: "Full page images", Unit: "images/s", Group: "wal",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_collector_wal_fpi"},
		Summary: "Whole 8 KiB pages written into WAL after a checkpoint.",
		Note: Note{
			Means: "The first time a page is modified after a checkpoint, PostgreSQL writes the entire page into the WAL rather than just the change, so recovery can survive a torn write. This counts those.",
			Why:   "Full page images are the single largest source of WAL volume on most instances, and they are directly controlled by checkpoint frequency.",
			Watch: "A high FPI share means checkpoints are too frequent: each one resets the 'first touch' state, so the next writes re-image the same pages. Lengthening checkpoint_timeout and raising max_wal_size reduce FPI volume, which reduces WAL, which reduces archiving and replication load — one change, three wins.",
		},
	},
	{
		Key: "wal-buffers-full", Title: "WAL buffers full", Unit: "events/s", Group: "wal",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_collector_wal_buffers_full"},
		Summary: "Times WAL had to be flushed because the buffer filled.",
		Note: Note{
			Means: "Occasions when a backend had to write WAL out because wal_buffers had no room left — an unplanned flush in the middle of someone's transaction.",
			Why:   "It is a small, cheap-to-fix source of latency that is invisible in every other metric.",
			Watch: "Sustained non-zero values mean wal_buffers is undersized for the write rate. It is one of the few PostgreSQL settings where raising it has essentially no downside — the memory involved is a few megabytes.",
		},
	},
	{
		Key: "wal-disk-size", Title: "WAL on the data volume", Unit: "bytes", Group: "wal",
		Kind: Gauge, Aggregate: Sum, Match: map[string]string{"value": "size"},
		Names:   []string{"cnpg_collector_pg_wal"},
		Summary: "Bytes of WAL segments currently sitting in pg_wal.",
		Note: Note{
			Means: "The total size of the WAL segment files on the instance's own volume — segments not yet archived, plus those retained for replication slots, plus the ones kept for recycling.",
			Why:   "pg_wal shares the data volume. WAL that cannot leave accumulates here, and when the volume fills PostgreSQL shuts down rather than corrupt itself.",
			Watch: "This should oscillate inside a band set by max_wal_size and wal_keep_size. A line that only rises means WAL is being retained and not released — archiving is failing, or an inactive replication slot is holding it. Both are visible in the sections below, and both end the same way if ignored: a full volume and a stopped primary.",
		},
	},

	// --------------------------------------------------- archive
	{
		Key: "wal-archive-ready", Title: "WAL segments waiting to archive", Unit: "segments", Group: "archive",
		Kind: Gauge, Aggregate: Sum, Match: map[string]string{"value": "ready"},
		Names:   []string{"cnpg_collector_pg_wal_archive_status"},
		Summary: "Segments marked ready in archive_status but not yet shipped.",
		Note: Note{
			Means: "The backlog: files the instance has finished and handed to the archiver, which the archive command has not yet confirmed as stored.",
			Why:   "This is the best single indicator of backup health on the whole screen. It is a queue depth, and a queue depth tells you about the future, not just the present.",
			Watch: "A healthy instance keeps this at zero to single digits, spiking briefly under write bursts. A line that only climbs means archiving is failing or is slower than WAL generation — and until it drains, none of that WAL can be recycled, so the data volume fills and the recovery window stops advancing. Both consequences are worse than the alert that precedes them.",
		},
	},
	{
		Key: "wal-archived", Title: "WAL segments archived", Unit: "segments/s", Group: "archive",
		Kind: Counter, Aggregate: Sum, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_archiver_archived_count_total", "cnpg_pg_stat_archiver_archived_count"},
		Summary: "Rate at which segments are successfully reaching the object store.",
		Note: Note{
			Means: "Successful archive operations per second, from pg_stat_archiver. Only the primary archives, so only the primary reports it.",
			Why:   "It is the drain rate for the backlog above. The two together say whether the queue is shrinking or growing.",
			Watch: "Zero while segments are waiting is a stalled archiver — credentials, network policy, or the object store itself. Compare the rate against the WAL generation rate: if archiving is slower on average, the backlog is unbounded no matter how healthy each individual transfer looks.",
		},
	},
	{
		Key: "wal-archive-failed", Title: "Failed archive attempts", Unit: "failures/s", Group: "archive",
		Kind: Counter, Aggregate: Sum, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_archiver_failed_count_total", "cnpg_pg_stat_archiver_failed_count"},
		Summary: "Rate of archive attempts that returned an error.",
		Note: Note{
			Means: "Failed archive operations per second. PostgreSQL retries a failed segment indefinitely, so one broken segment can produce a steady failure rate on its own.",
			Why:   "Failures are what turn the backlog above from a spike into a trend.",
			Watch: "Any sustained non-zero rate means archiving is broken right now, and the recovery point is frozen at the last success — which the tile beside this names. A backup taken while this is failing is not a recovery point: point-in-time recovery needs the WAL chain, not just the base backup.",
		},
	},

	// ----------------------------------------------- replication
	{
		Key: "replication-lag", Title: "Replication lag", Unit: "s", Group: "replication",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_replication_lag"},
		Summary: "Seconds this replica is behind the primary, as the replica sees it.",
		Note: Note{
			Means: "The replica's own view of how stale its data is. A primary reports zero because it is not in recovery.",
			Why:   "It is the number that decides whether reads on this replica are acceptable, and how much data a failover to it would lose.",
			Watch: "Lag that grows without bound means the replica cannot apply WAL as fast as the primary makes it — usually a saturated replica disk, or replay blocked behind a long-running query on the replica. Sustained lag also means the primary's slot is retaining WAL for it, so the primary's own volume starts filling in sympathy.",
		},
	},
	{
		Key: "replay-lag-seconds", Title: "Replay lag, primary's view", Unit: "s", Group: "replication",
		Kind: Gauge, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_replay_lag_seconds"},
		Summary: "Worst replay delay across the standbys, measured from the primary.",
		Note: Note{
			Means: "How long the primary waited between flushing WAL locally and hearing that a standby had applied it. The console keeps the worst standby's figure.",
			Why:   "The primary's view covers every standby at once, including one that has stopped reporting for itself. It is the number a synchronous commit would actually block on.",
			Watch: "Replay lag rising while write lag stays flat means WAL is arriving at the standby fine and being applied slowly — the standby's disk, or replay blocked by a reader. That distinction decides whether you look at the network or at the replica.",
		},
	},
	{
		Key: "replay-diff-bytes", Title: "Replay backlog", Unit: "bytes", Group: "replication",
		Kind: Gauge, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_replay_diff_bytes"},
		Summary: "Bytes of WAL the furthest-behind standby has yet to apply.",
		Note: Note{
			Means: "The byte distance between the primary's current WAL position and the position a standby has replayed up to.",
			Why:   "Seconds of lag depend on the write rate; bytes do not. During an idle period a replica can show hours of 'lag' while being one byte behind, and this is the line that says so.",
			Watch: "Read this with the seconds figure. Bytes near zero and seconds large means the primary is simply quiet. Bytes large and growing is a real backlog, and it is also the amount of data a failover would lose.",
		},
	},
	{
		Key: "slot-retained-bytes", Title: "WAL retained by slots", Unit: "bytes", Group: "replication",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_replication_slots_pg_wal_lsn_diff"},
		Summary: "WAL the worst replication slot is pinning on this instance.",
		Note: Note{
			Means: "How far behind the current WAL position a replication slot's confirmed position sits. The instance may not recycle any WAL past that point.",
			Why:   "This is the most common way a PostgreSQL volume fills. A slot's entire purpose is to retain WAL, and a slot whose consumer went away retains it forever.",
			Watch: "A slot belonging to a running replica tracks its lag and falls back. A slot growing steadily with no consumer — a replica deleted without dropping its slot, a logical subscriber that stopped — will consume the data volume until the instance shuts down. Check the slot's active flag before assuming this is replication lag.",
		},
	},
	{
		Key: "streaming-replicas", Title: "Streaming replicas connected", Unit: "replicas", Group: "replication",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_replication_streaming_replicas"},
		Summary: "Standbys currently streaming from this instance.",
		Note: Note{
			Means: "The number of walsender connections the instance has. Only a primary normally has any, unless cascading replication is in use.",
			Why:   "It is the fastest way to see a replica disconnect. Lag metrics go quiet when a replica leaves; this one drops.",
			Watch: "A drop that does not recover means a standby is gone, and its slot — if it has one — is now retaining WAL for a consumer that will not return. Redundancy is reduced immediately; the volume pressure follows later.",
		},
	},

	// --------------------------------------------------- storage
	{
		Key: "database-size", Title: "Database size", Unit: "bytes", Group: "storage",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pg_database_size_bytes"},
		Summary: "Disk occupied by the databases on this instance.",
		Note: Note{
			Means: "The sum of pg_database_size across every database, which is tables, indexes, TOAST and free space inside them. It does not include WAL.",
			Why:   "It is the growth curve the volume has to accommodate, and the input to every capacity decision.",
			Watch: "A size that grows while rows do not is bloat, not data: dead row versions vacuum has not reclaimed. Deletes that do not shrink it are working as designed — space is returned to the table, not to the filesystem. Extrapolate the slope against the volume size; storage expansion is not instant on every platform.",
		},
	},
	{
		Key: "xid-age", Title: "Transaction id age", Unit: "transactions", Group: "storage",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_database_xid_age"},
		Summary: "Transactions since the oldest unfrozen row in the worst database.",
		Note: Note{
			Means: "The distance between the current transaction id and the oldest one still unfrozen. PostgreSQL's transaction id space is 32 bits and wraps; freezing is what keeps old rows readable across the wrap.",
			Why:   "Wraparound is one of the few PostgreSQL failure modes that stops writes completely, and it arrives on a schedule you can see coming for weeks.",
			Watch: "A healthy instance sawtooths as autovacuum freezes old rows. A line that only climbs means freezing is not completing — usually blocked by a long-open transaction, an abandoned replication slot, or a prepared transaction left behind. Past roughly 200 million PostgreSQL forces aggressive autovacuum; near 2 billion it refuses new write transactions until an offline VACUUM is run.",
		},
	},
	{
		Key: "mxid-age", Title: "Multixact id age", Unit: "multixacts", Group: "storage",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_database_mxid_age"},
		Summary: "The same wraparound clock, for multixact ids.",
		Note: Note{
			Means: "Age of the oldest unfrozen multixact id. Multixacts are allocated when several transactions hold row-level locks on the same row at once — typically foreign key checks and SELECT FOR SHARE.",
			Why:   "It wraps around exactly like the transaction id counter, with the same consequence, and it is checked far less often because most workloads never move it.",
			Watch: "Usually flat. A climbing multixact age points at heavy concurrent row locking, and it reaches the same shutdown-to-protect-data outcome as transaction ids do. Worth a glance whenever xid age is being investigated, since the same blockers stall both.",
		},
	},
	// --------------------------------------------------- volumes
	// cnpg_disk_* (FORK origin/dev/60 ed17f177f
	// pkg/management/postgres/webserver/metricserver/disk.go; the byte
	// names are those of upstream CloudNativePG draft PR 11325). Gauges with a volume
	// label: pgdata, wal, tbs-<name>. used = blocks - bfree (reserved
	// blocks included), available = bavail (reserved excluded): the
	// volume is full for PostgreSQL when available reaches zero.
	diskSeries("pgdata", "Data volume", "available", "the data volume (PGDATA)"),
	diskSeries("pgdata", "Data volume", "used", "the data volume (PGDATA)"),
	diskSeries("pgdata", "Data volume", "inodes-free", "the data volume (PGDATA)"),
	diskSeries("wal", "WAL volume", "available", "the separate WAL volume"),
	diskSeries("wal", "WAL volume", "used", "the separate WAL volume"),
	diskSeries("wal", "WAL volume", "inodes-free", "the separate WAL volume"),
}

// diskSeries is one cnpg_disk_* series of one fixed volume. Tablespace
// volumes (tbs-<name>) are not charted here: their names are the
// Cluster's own, and the storage screen's forecast lists them per volume.
func diskSeries(volume, title, measure, what string) SeriesDef {
	def := SeriesDef{
		Key: "disk-" + volume + "-" + measure, Group: "volumes", Kind: Gauge, Aggregate: Max,
		Match: map[string]string{"volume": volume},
	}
	switch measure {
	case "available":
		def.Title, def.Unit, def.Names = title+" — available", "bytes", []string{"cnpg_disk_available_bytes"}
		def.Summary = "Bytes PostgreSQL can still write on " + what + "."
		def.Note = Note{
			Means: "statfs's bavail times the block size: free space a non-root process may use, the filesystem's reserved blocks excluded. PostgreSQL runs as such a process.",
			Why:   "When it reaches zero PostgreSQL cannot extend a file: a write fails with \"No space left on device\", and on the WAL path that is a PANIC that stops the instance.",
			Watch: "A steady decline is growth; extrapolate it against the line. A sudden drop is a burst of WAL (a slot holding it, archiving failing, a large batch) or a bulk load. A sawtooth that climbs back is WAL being recycled after checkpoints.",
		}
	case "used":
		def.Title, def.Unit, def.Names = title+" — used", "bytes", []string{"cnpg_disk_used_bytes"}
		def.Summary = "Bytes in use on " + what + ", reserved blocks included."
		def.Note = Note{
			Means: "statfs's blocks minus bfree, times the block size. It counts the blocks the filesystem reserves for root, so used plus available is less than the size.",
			Why:   "It is the growth curve without the noise of a volume being resized: a PVC expansion moves available, not used.",
			Watch: "Compare with the database size: the difference on the data volume is WAL (when it shares the volume), temporary files and filesystem overhead. A used figure that grows while the databases do not is usually WAL being retained.",
		}
	default:
		def.Title, def.Unit, def.Names = title+" — free inodes", "inodes", []string{"cnpg_disk_inodes_free"}
		def.Summary = "Inodes left on " + what + "; absent when the filesystem reports none."
		def.Note = Note{
			Means: "statfs's ffree: how many more files the filesystem can create. Not reported on filesystems without a fixed inode count.",
			Why:   "A volume can run out of inodes with bytes to spare, and then fails exactly like a full one. Many small relations (partitions, TOAST, temporary files) or a WAL directory full of small segments are the usual cause.",
			Watch: "A decline with bytes still free is the case byte charts miss. Free inodes reaching zero stops file creation: no new relation, segment or temporary file.",
		}
	}
	return def
}

// Render says how an instant's number becomes text. The store keeps
// float64s; the rendering decision belongs with the definition, because
// a unix timestamp and a page count are the same float and must never
// be shown the same way.
type Render int

const (
	// RenderNumber prints the value with four significant digits.
	RenderNumber Render = iota
	// RenderBytes humanises a byte count.
	RenderBytes
	// RenderSeconds renders a duration given in seconds.
	RenderSeconds
	// RenderTimestamp renders unix seconds as an absolute time.
	RenderTimestamp
	// RenderWords maps zero and non-zero onto InstantDef.Words.
	RenderWords
	// RenderVersion prints the value as reported, trimmed, e.g. "18.4".
	RenderVersion
)

// InstantDef is one point-in-time tile: a number whose latest value is
// the whole reading. It costs one sample per instance, so the catalog
// can be broad where Catalog must be selective.
type InstantDef struct {
	// Key is the stable identifier used in storage and markup.
	Key string
	// Title is the tile's label.
	Title string
	// Unit suffixes the value where a bare number would be ambiguous.
	Unit string
	// Group is the GroupDef key this tile renders under.
	Group string
	// Summary is the one-line gloss shown under the title.
	Summary string
	// Note is the disclosed explanation.
	Note Note
	// Render selects the formatting.
	Render Render
	// Words are the zero and non-zero readings for RenderWords.
	Words [2]string
	// ZeroMeansNever marks a timestamp whose zero value is "never
	// happened" rather than the Unix epoch.
	ZeroMeansNever bool
	// Aggregate folds label sets into one value.
	Aggregate Aggregate
	// Scope says which instances report it at all.
	Scope Scope
	// Match, when set, keeps only matching label sets.
	Match map[string]string
	// Names are the candidate metric names, first match wins.
	Names []string
}

// instanceInstants is the instance exporter's point-in-time surface.
var instanceInstants = []InstantDef{
	// -------------------------------------------------- instance
	{
		Key: "postgres-version", Title: "PostgreSQL version", Group: "instance",
		Render: RenderVersion, Aggregate: Max, Names: []string{"cnpg_collector_postgres_version"},
		Summary: "The major.minor version this instance reports.",
		Note: Note{
			Means: "The version the running postmaster reports, not the version in the image reference — after a minor upgrade the two differ until the pod restarts.",
			Why:   "Minor releases carry data-corruption and security fixes, and every instance in a cluster should be on the same one.",
			Watch: "Instances disagreeing means a rolling upgrade is in progress or stalled part-way. A cluster left split across minor versions works, but replication between distant versions is not something to leave running unattended.",
		},
	},
	{
		Key: "instance-up", Title: "PostgreSQL responding", Group: "instance",
		Render: RenderWords, Words: [2]string{"not responding", "responding"}, Aggregate: Max,
		Names:   []string{"cnpg_collector_up"},
		Summary: "Whether the exporter could reach PostgreSQL on its last collection.",
		Note: Note{
			Means: "The exporter's own probe: 1 when it connected and ran its queries, 0 when it could not. It is a claim about the exporter's last attempt, not a health verdict.",
			Why:   "It separates 'the database is down' from 'the metrics are stale'. If this is 0, every other number on this tab is the last thing the exporter managed to read, not the current state.",
			Watch: "Not responding while the pod is Running means postmaster is up but not answering — starting up, in crash recovery, or out of connection slots. Treat every other reading on this tab as suspect until it returns.",
		},
	},
	{
		Key: "in-recovery", Title: "In recovery", Group: "instance",
		Render: RenderWords, Words: [2]string{"no — accepting writes", "yes — replica"}, Aggregate: Max,
		Names:   []string{"cnpg_pg_replication_in_recovery"},
		Summary: "Whether this instance is replaying WAL rather than generating it.",
		Note: Note{
			Means: "PostgreSQL's own answer to pg_is_in_recovery(). It is the database's view of its role, which is not necessarily the operator's view or the Service's.",
			Why:   "Role confusion is the root of a whole class of incidents. This is the instance speaking for itself, independent of any label or endpoint.",
			Watch: "If this disagrees with the role the cluster overview shows, believe neither until you know why: a stale label, a failover in progress, or a split brain all look like this from one tab. Two instances both reporting 'accepting writes' is the serious case.",
		},
	},
	{
		Key: "wal-receiver-up", Title: "WAL receiver", Group: "instance",
		Render: RenderWords, Words: [2]string{"not running", "running"}, Aggregate: Max,
		Names:   []string{"cnpg_pg_replication_is_wal_receiver_up"},
		Summary: "Whether this replica has a live connection pulling WAL.",
		Note: Note{
			Means: "Whether the walreceiver process is connected to an upstream. A primary has none, which reads as not running and is correct.",
			Why:   "A replica with no WAL receiver is not replicating. It will still serve reads, and it will still look healthy, while its data quietly ages.",
			Watch: "On a replica, not running means streaming has stopped — the replica may be restoring from the archive instead, or it may be stuck. Lag will climb from here, and a failover to this instance would lose everything since the receiver stopped.",
		},
	},
	{
		Key: "postmaster-start", Title: "PostgreSQL started", Group: "instance",
		Render: RenderTimestamp, Aggregate: Max, Names: []string{"cnpg_pg_postmaster_start_time"},
		Summary: "When this postmaster process started.",
		Note: Note{
			Means: "The instance's own start time. It resets on every restart, including one the operator performed and one the kernel forced.",
			Why:   "It dates the cache: everything in shared_buffers has been accumulated since this moment, and so has every cumulative counter on this screen.",
			Watch: "A start time more recent than you expected is a restart you did not know about — check the pod's restart count. It also explains a temporarily poor cache hit ratio and a reset in every counter, neither of which is a fault.",
		},
	},
	{
		Key: "fencing-on", Title: "Fenced", Group: "instance",
		Render: RenderWords, Words: [2]string{"no", "yes — instance is fenced"}, Aggregate: Max,
		Names:   []string{"cnpg_collector_fencing_on"},
		Summary: "Whether the operator has fenced this instance.",
		Note: Note{
			Means: "Fencing is a CloudNativePG operation that stops PostgreSQL while leaving the pod and its volume in place, so the instance can be worked on without the operator restarting it.",
			Why:   "A fenced instance is deliberately not serving. Without this tile it looks identical to a broken one.",
			Watch: "Fencing is intentional and is meant to be temporary. An instance left fenced is one fewer replica, so the cluster's redundancy is lower than its instance count suggests, and a fenced primary is an outage.",
		},
	},
	{
		Key: "replica-mode", Title: "Replica cluster mode", Group: "instance",
		Render: RenderWords, Words: [2]string{"no", "yes — whole cluster is a replica"}, Aggregate: Max,
		Names:   []string{"cnpg_collector_replica_mode"},
		Summary: "Whether the whole cluster is a replica of another cluster.",
		Note: Note{
			Means: "CloudNativePG replica-cluster mode: every instance here follows an external source, and none of them accepts writes.",
			Why:   "It changes what 'primary' means on this screen. The designated primary of a replica cluster is still read-only.",
			Watch: "If this is yes and an application expects to write here, it will fail on every write until the cluster is promoted. Promotion is a deliberate operation, not something a failover does.",
		},
	},
	{
		Key: "switchover-required", Title: "Manual switchover required", Group: "instance",
		Render: RenderWords, Words: [2]string{"no", "yes"}, Aggregate: Max,
		Names:   []string{"cnpg_collector_manual_switchover_required"},
		Summary: "Whether the operator is waiting for a human to switch over.",
		Note: Note{
			Means: "CloudNativePG sets this when it needs the primary role to move but its update strategy is supervised, so it will not move it on its own.",
			Why:   "It marks a rollout that has stopped and will not restart by itself. Nothing is broken and nothing is progressing.",
			Watch: "While this is set, the cluster is part-way through an update: instances may be on different versions or configurations, and the pending work does not resume until a switchover is performed.",
		},
	},
	{
		Key: "nodes-used", Title: "Distinct nodes in use", Unit: "nodes", Group: "instance",
		Render: RenderNumber, Aggregate: Max, Names: []string{"cnpg_collector_nodes_used"},
		Summary: "How many Kubernetes nodes the instances are spread across.",
		Note: Note{
			Means: "The count of distinct nodes hosting this cluster's instances. CloudNativePG reports -1 when it cannot tell.",
			Why:   "Three instances on one node is not high availability, however healthy every replication metric looks.",
			Watch: "A value of 1 with more than one instance means a single node failure takes the whole cluster. It should equal the instance count; anything less is anti-affinity that did not take effect, usually because the scheduler had nowhere else to place the pod.",
		},
	},
	{
		Key: "sync-replicas-expected", Title: "Synchronous replicas expected", Unit: "replicas", Group: "instance",
		Render: RenderNumber, Aggregate: Max, Match: map[string]string{"value": "expected"},
		Names:   []string{"cnpg_collector_sync_replicas"},
		Summary: "How many synchronous standbys the configuration asks for.",
		Note: Note{
			Means: "The number in synchronous_standby_names — how many standbys must confirm a write before the primary tells the client it committed.",
			Why:   "It is the durability contract. Zero means commits are acknowledged as soon as they are local, so a primary loss can lose recently committed transactions.",
			Watch: "Compare against the observed count beside it. Expected above observed means the primary is either blocking on commits or has quietly relaxed the requirement, depending on how the cluster is configured — and the two behaviours have very different failure modes.",
		},
	},
	{
		Key: "sync-replicas-observed", Title: "Synchronous replicas observed", Unit: "replicas", Group: "instance",
		Render: RenderNumber, Aggregate: Max, Match: map[string]string{"value": "observed"},
		Names:   []string{"cnpg_collector_sync_replicas"},
		Summary: "How many synchronous standbys are actually confirming writes.",
		Note: Note{
			Means: "The count of standbys currently meeting the synchronous requirement.",
			Why:   "This is the durability you have, as opposed to the durability you asked for.",
			Watch: "Below the expected count, the guarantee is not being met right now. Depending on configuration the primary is either stalling every commit until a standby returns — which looks like a total outage — or proceeding without the guarantee, which looks like nothing at all until you lose the primary.",
		},
	},

	// --------------------------------------------------- storage
	{
		Key: "lo-pages", Title: "Large object pages", Unit: "pages", Group: "storage",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_collector_lo_pages"},
		Summary: "Estimated pages in pg_largeobject.",
		Note: Note{
			Means: "An estimate of how many 8 KiB pages the large object table occupies, summed across databases.",
			Why:   "Large objects are easy to create and easy to orphan: deleting the row that referenced one does not delete the object.",
			Watch: "Growth here with no application that knowingly uses large objects means orphans are accumulating, and they are invisible in ordinary table sizes. vacuumlo is the tool that removes them.",
		},
	},
	{
		Key: "extensions-update", Title: "Extension updates available", Unit: "extensions", Group: "storage",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pg_extensions_update_available"},
		Summary: "Installed extensions whose default version is newer than the installed one.",
		Note: Note{
			Means: "A count of extensions where the shared library on disk offers a newer version than the one registered in the database.",
			Why:   "An image upgrade updates the library but not the catalog: the database keeps running the old SQL definitions until someone runs ALTER EXTENSION ... UPDATE.",
			Watch: "A non-zero count after an image upgrade is expected and is a job left to do. Until it is done, fixes shipped in the new extension version are present on disk and not in effect.",
		},
	},

	// ------------------------------------------------------- wal
	{
		Key: "wal-segments", Title: "WAL segments on disk", Unit: "segments", Group: "wal",
		Render: RenderNumber, Aggregate: Sum, Match: map[string]string{"value": "count"},
		Names:   []string{"cnpg_collector_pg_wal"},
		Summary: "Number of segment files currently in pg_wal.",
		Note: Note{
			Means: "The file count behind the WAL size chart. At the default 16 MiB per segment, the two track each other exactly.",
			Why:   "Counts are easier to reason about against min/max settings than byte totals are.",
			Watch: "Read against the min and max tiles: sitting at max means the instance is at its WAL ceiling and checkpoints are being forced by volume rather than by time.",
		},
	},
	{
		Key: "wal-segments-min", Title: "WAL segments — floor", Unit: "segments", Group: "wal",
		Render: RenderNumber, Aggregate: Max, Match: map[string]string{"value": "min"},
		Names:   []string{"cnpg_collector_pg_wal"},
		Summary: "Segments the instance keeps even when idle (min_wal_size).",
		Note: Note{
			Means: "The floor PostgreSQL will not recycle below, so a burst of writes finds files ready rather than having to create them.",
			Why:   "It is why pg_wal never empties on an idle instance — that is the setting working, not a leak.",
			Watch: "Nothing to act on by itself; it is the baseline the count above should return to after a burst. A count that never comes back down to roughly this level is the signal.",
		},
	},
	{
		Key: "wal-segments-max", Title: "WAL segments — ceiling", Unit: "segments", Group: "wal",
		Render: RenderNumber, Aggregate: Max, Match: map[string]string{"value": "max"},
		Names:   []string{"cnpg_collector_pg_wal"},
		Summary: "Segments allowed before a checkpoint is forced (max_wal_size).",
		Note: Note{
			Means: "The soft ceiling on WAL between checkpoints. Reaching it triggers a requested checkpoint.",
			Why:   "It is the setting behind the requested-checkpoint chart, and the one to change when requested checkpoints outnumber scheduled ones.",
			Watch: "It is a soft limit: WAL that cannot be recycled — unarchived, or held by a slot — goes past it freely. So a pg_wal much larger than this ceiling is not a tuning question, it is a retention problem.",
		},
	},
	{
		Key: "wal-segments-keep", Title: "WAL segments retained", Unit: "segments", Group: "wal",
		Render: RenderNumber, Aggregate: Max, Match: map[string]string{"value": "keep"},
		Names:   []string{"cnpg_collector_pg_wal"},
		Summary: "Segments deliberately kept for standbys (wal_keep_size).",
		Note: Note{
			Means: "Segments retained beyond what the instance needs itself, so a standby that briefly disconnects can catch up by streaming instead of restoring from the archive.",
			Why:   "It is a floor on disk usage bought in exchange for smoother replica recovery.",
			Watch: "It bounds how long a standby may be away before it must fall back to the archive. Set generously, it is a permanent disk cost; set to zero, a brief network partition can force a replica to restore.",
		},
	},

	// --------------------------------------------------- archive
	{
		Key: "archive-done", Title: "WAL segments archived (marked done)", Unit: "segments", Group: "archive",
		Render: RenderNumber, Aggregate: Sum, Match: map[string]string{"value": "done"},
		Names:   []string{"cnpg_collector_pg_wal_archive_status"},
		Summary: "Segments in archive_status confirmed stored and awaiting cleanup.",
		Note: Note{
			Means: "Status files for segments the archiver has finished with. PostgreSQL removes them shortly after; a small number here is normal.",
			Why:   "It is the counterpart to the ready count — together they show the queue draining.",
			Watch: "Meaningful only next to the ready count. Ready high and done unchanging means nothing is being archived at all.",
		},
	},
	{
		Key: "last-archived-time", Title: "Last successful archive", Group: "archive",
		Render: RenderTimestamp, Aggregate: Max, Scope: ScopePrimary, ZeroMeansNever: true,
		Names:   []string{"cnpg_pg_stat_archiver_last_archived_time"},
		Summary: "When a WAL segment last reached the object store.",
		Note: Note{
			Means: "The timestamp of the most recent successful archive operation, from pg_stat_archiver.",
			Why:   "It is the honest edge of the recovery window as far as the instance can tell. Point-in-time recovery cannot reach past it.",
			Watch: "If this is older than your recovery point objective, the objective is not being met right now — regardless of what any backup schedule claims. Note this is the instance's claim about shipping a file; only the evidence agent can say what the repository actually holds.",
		},
	},
	{
		Key: "seconds-since-archival", Title: "Time since last archive", Group: "archive",
		Render: RenderSeconds, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_archiver_seconds_since_last_archival"},
		Summary: "How long ago the last segment was successfully archived.",
		Note: Note{
			Means: "The same fact as the timestamp above, already expressed as an age by the exporter.",
			Why:   "An age is easier to alert on than a timestamp, and it does not depend on the reader's clock.",
			Watch: "Compare against how fast this instance fills a segment. On a busy instance a gap of many minutes means archiving has stalled; on an idle one it may simply mean there was nothing to ship — archive_timeout is what bounds that case.",
		},
	},
	{
		Key: "last-failed-archive", Title: "Last failed archive", Group: "archive",
		Render: RenderTimestamp, Aggregate: Max, Scope: ScopePrimary, ZeroMeansNever: true,
		Names:   []string{"cnpg_pg_stat_archiver_last_failed_time"},
		Summary: "When an archive attempt last returned an error.",
		Note: Note{
			Means: "The timestamp of the most recent failed archive operation. It is never cleared by later successes — only by a statistics reset.",
			Why:   "It dates a problem. A failure timestamp from last month next to a recent success is history; one from a minute ago is an incident.",
			Watch: "A failure timestamp newer than the success timestamp means archiving is broken right now, and every segment since is still on the data volume.",
		},
	},
	{
		Key: "last-backup", Title: "Last available backup", Group: "archive",
		Render: RenderTimestamp, Aggregate: Max, ZeroMeansNever: true,
		Names: []string{
			"barman_cloud_cloudnative_pg_io_last_available_backup_timestamp",
			"cnpg_collector_last_available_backup_timestamp",
		},
		Summary: "When the most recent base backup completed, as the plugin reports it.",
		Note: Note{
			Means: "The barman-cloud plugin's claim about the newest base backup it knows of. The cnpg_collector_ spelling of this metric is deprecated in CloudNativePG 1.30 and is read only as a fallback.",
			Why:   "A base backup plus the WAL after it is what a restore needs. Neither half is sufficient alone.",
			Watch: "An age beyond the backup schedule's period means backups are not running as configured. This is the operator's claim that a backup was taken — that it would restore is something only the evidence agent can speak to, and the console does not infer it.",
		},
	},
	{
		Key: "last-failed-backup", Title: "Last failed backup", Group: "archive",
		Render: RenderTimestamp, Aggregate: Max, ZeroMeansNever: true,
		Names: []string{
			"barman_cloud_cloudnative_pg_io_last_failed_backup_timestamp",
			"cnpg_collector_last_failed_backup_timestamp",
		},
		Summary: "When a backup attempt last failed.",
		Note: Note{
			Means: "The plugin's timestamp for the most recent failed backup attempt. Zero means none has been recorded.",
			Why:   "Backups fail quietly. A schedule that fires and fails looks the same from outside as one that never fired.",
			Watch: "Newer than the last successful backup means the most recent attempt failed and the newest usable base backup is older than the schedule implies.",
		},
	},
	{
		Key: "first-recoverability", Title: "First recoverability point", Group: "archive",
		Render: RenderTimestamp, Aggregate: Max, ZeroMeansNever: true,
		Names: []string{
			"barman_cloud_cloudnative_pg_io_first_recoverability_point",
			"cnpg_collector_first_recoverability_point",
		},
		Summary: "The oldest point in time the repository can restore to.",
		Note: Note{
			Means: "The start of the recovery window the plugin believes it holds — the earliest timestamp a point-in-time restore could target.",
			Why:   "With the last backup it brackets the recovery window: everything before this is gone, whatever the retention policy says.",
			Watch: "A first point that jumps forward means retention pruned older backups. If it moves past a date you are required to be able to restore, the retention policy and the compliance requirement disagree, and the policy is winning.",
		},
	},

	// ----------------------------------------------- replication
	{
		Key: "sent-diff-bytes", Title: "Send backlog", Unit: "bytes", Group: "replication",
		Render: RenderBytes, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_sent_diff_bytes"},
		Summary: "WAL written locally but not yet sent to the worst standby.",
		Note: Note{
			Means: "The gap between the primary's current WAL position and what it has put on the wire for a standby.",
			Why:   "It is the first of four stages — sent, written, flushed, replayed — and it isolates the primary's own sending from everything downstream.",
			Watch: "A backlog here, before the network is even involved, points at the primary: a saturated walsender or a stalled connection. If sent is near zero and replay is far behind, the problem is at the other end.",
		},
	},
	{
		Key: "write-diff-bytes", Title: "Write backlog", Unit: "bytes", Group: "replication",
		Render: RenderBytes, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_write_diff_bytes"},
		Summary: "WAL sent but not yet written to the standby's disk.",
		Note: Note{
			Means: "The gap between what the primary sent and what the standby confirms it has written to its operating system.",
			Why:   "It separates network transfer from standby storage.",
			Watch: "A growing write backlog while the send backlog stays flat means the bytes are in flight or queued at the standby — a slow link or a busy replica.",
		},
	},
	{
		Key: "flush-diff-bytes", Title: "Flush backlog", Unit: "bytes", Group: "replication",
		Render: RenderBytes, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_flush_diff_bytes"},
		Summary: "WAL written on the standby but not yet durable there.",
		Note: Note{
			Means: "The gap between written and fsynced on the standby. Synchronous commit with the default setting waits precisely for this to close.",
			Why:   "It is the exact quantity a synchronous primary blocks on, so it is where synchronous commit latency comes from.",
			Watch: "Sustained flush backlog on a synchronous standby means every commit on the primary is waiting on that standby's fsync. The primary's write latency becomes the standby's disk latency, and no primary-side tuning will change it.",
		},
	},
	{
		Key: "backend-xmin-age", Title: "Standby xmin horizon age", Unit: "transactions", Group: "replication",
		Render: RenderNumber, Aggregate: Max, Scope: ScopePrimary,
		Names:   []string{"cnpg_pg_stat_replication_backend_xmin_age"},
		Summary: "How far back a standby is holding the primary's cleanup horizon.",
		Note: Note{
			Means: "The age of the oldest transaction id a standby has asked the primary to preserve. Non-zero only when hot_standby_feedback is on.",
			Why:   "hot_standby_feedback stops replicas cancelling queries by making the primary keep the rows those queries need. This is the bill for that.",
			Watch: "A large, growing value means a long query on a replica is holding vacuum back on the primary. Bloat and xid age grow on the primary because of a read on the replica — a connection that surprises people, and the storage section is where the damage appears.",
		},
	},
	{
		Key: "slots-active", Title: "Active replication slots", Unit: "slots", Group: "replication",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pg_replication_slots_active"},
		Summary: "Replication slots with a consumer currently attached.",
		Note: Note{
			Means: "The sum of the active flag across this instance's replication slots: how many have something connected.",
			Why:   "Read against the WAL-retained-by-slots chart, it distinguishes a slot doing its job from a slot abandoned by its consumer.",
			Watch: "A slot that is inactive while retaining WAL is the dangerous case: nothing will ever consume it, and it holds WAL on the data volume until someone drops it. That is the most common cause of a PostgreSQL volume filling for no visible reason.",
		},
	},

	// --------------------------------------------------- volumes
	diskInstant("pgdata", "Data volume size", "cnpg_disk_total_bytes", RenderBytes),
	diskInstant("pgdata", "Data volume inodes", "cnpg_disk_inodes", RenderNumber),
	diskInstant("wal", "WAL volume size", "cnpg_disk_total_bytes", RenderBytes),
	diskInstant("wal", "WAL volume inodes", "cnpg_disk_inodes", RenderNumber),
}

// diskInstant is the size or the inode count of one fixed volume: they
// move only when the volume is resized, so a tile, not a chart.
func diskInstant(volume, title, name string, render Render) InstantDef {
	def := InstantDef{
		Key: "disk-" + volume + "-inodes", Title: title, Group: "volumes",
		Render: render, Aggregate: Max, Match: map[string]string{"volume": volume}, Names: []string{name},
	}
	if render == RenderBytes {
		def.Key = "disk-" + volume + "-size"
		def.Summary = "The filesystem's size, as statfs reports it."
		def.Note = Note{
			Means: "statfs's blocks times the block size: the filesystem, which is a little smaller than the PersistentVolumeClaim's capacity.",
			Why:   "It is the line the available chart runs down towards, and it changes when the volume is expanded (by hand or by the fork's autoGrow).",
			Watch: "A size that grew while the PVC request did not means the volume was expanded outside the Cluster spec; one that did not grow after a resize means the filesystem expansion is still pending on the node.",
		}
	} else {
		def.Unit = "inodes"
		def.Summary = "The filesystem's inode count; absent when it reports none."
		def.Note = Note{
			Means: "statfs's files: the number of files the filesystem can hold at its current size.",
			Why:   "It is the ceiling of the free-inodes chart.",
			Watch: "Fixed on ext4 until the volume is expanded; on XFS it follows free space, so it moves with the bytes.",
		}
	}
	return def
}

// Limits bound the store. Zero values take the defaults.
type Limits struct {
	// Interval is the scrape cadence; it sizes the raw tier.
	Interval time.Duration
	// RawWindow is how long raw samples are kept.
	RawWindow time.Duration
	// Retention is how long rollups are kept.
	Retention time.Duration
	// RollupEvery is the rollup bucket width.
	RollupEvery time.Duration
	// MaxInstances caps how many instances are tracked at once.
	MaxInstances int
}

// Default bounds: 10s scrapes, 6h of raw samples, 7d of 5-minute
// rollups, at most 8 instances.
const (
	DefaultInterval    = 10 * time.Second
	DefaultRawWindow   = 6 * time.Hour
	DefaultRetention   = 7 * 24 * time.Hour
	DefaultRollupEvery = 5 * time.Minute
	DefaultInstances   = 8
)

// withDefaults fills unset limits.
func (l Limits) withDefaults() Limits {
	if l.Interval <= 0 {
		l.Interval = DefaultInterval
	}
	if l.RawWindow <= 0 {
		l.RawWindow = DefaultRawWindow
	}
	if l.Retention <= 0 {
		l.Retention = DefaultRetention
	}
	if l.RollupEvery <= 0 {
		l.RollupEvery = DefaultRollupEvery
	}
	if l.MaxInstances <= 0 {
		l.MaxInstances = DefaultInstances
	}
	return l
}

// Point is one read-side sample. A nil Value is a gap: the console did
// not observe a claim there and says so.
type Point struct {
	// At is the sample time, Unix seconds.
	At int64
	// Value is the reported value or derived rate; nil is a gap.
	Value *float64
}

// Stats summarise one instrument over a window; nil fields mean no
// samples were observed.
type Stats struct {
	Latest *float64
	Min    *float64
	Max    *float64
	Avg    *float64
}

// Instant is one point-in-time reading and when it was observed. A zero
// At means the instance has never reported it.
type Instant struct {
	// At is the sweep time, Unix seconds.
	At int64
	// Value is what the exporter claimed.
	Value float64
}

// Pooler is the CloudNativePG PgBouncer exporter's surface, served on
// each pooler pod beside the instance exporter's own port.
//
// It answers a different question from the instance catalog, and the
// difference is the whole reason poolers exist: an instance reports on
// the work it is doing, a pooler reports on the queue in front of that
// work. Almost everything worth watching here is a queue depth or a
// wait — how many clients hold a server connection, how many are still
// asking for one, and how long the one at the front has waited.
//
// The console never connects to PgBouncer. Every number is a claim by
// the exporter running beside it, exactly as with the instances.
var Pooler = Catalog{Groups: poolerGroups, Series: poolerCharts, Instants: poolerInstants}

var poolerGroups = []GroupDef{
	{
		Key: "queue", Title: "Clients and the queue",
		Blurb: "Who is connected to the pooler, and who is still waiting for a server connection to work on.",
	},
	{
		Key: "servers", Title: "Server connections",
		Blurb: "The connections the pooler holds open to PostgreSQL, and what each of them is doing.",
	},
	{
		Key: "throughput", Title: "Throughput and latency",
		Blurb: "What the pooler is carrying, and how long the two halves of a round trip are taking.",
	},
	{
		Key: "capacity", Title: "Pooler capacity",
		Blurb: "The slots PgBouncer allocated for clients, servers and pools, and how much of each is spoken for.",
	},
}

var poolerCharts = []SeriesDef{
	// ----------------------------------------------------- queue
	{
		Key: "cl-active", Title: "Clients holding a server", Unit: "clients", Group: "queue",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_cl_active"},
		Summary: "Client connections linked to a server connection, able to run queries.",
		Note: Note{
			Means: "Clients that have been handed a server connection and can send work down it. Summed across every pool the pooler serves.",
			Why:   "This is the pooler doing its job: the number of clients actually able to make progress at this instant.",
			Watch: "Read it against the waiting line beside it. Active high and waiting at zero is a pool comfortably sized for its load. Active pinned at pool_size with a queue behind it is the pool at its ceiling — every extra client is waiting, not working.",
		},
	},
	{
		Key: "cl-waiting", Title: "Clients waiting for a server", Unit: "clients", Group: "queue",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_cl_waiting"},
		Summary: "Client connections that have sent a query and have no server connection yet.",
		Note: Note{
			Means: "Clients queued: they have work to send and are waiting for the pooler to free a server connection for them.",
			Why:   "It is the clearest saturation signal a pooler has. A connection pool exists to make this number small; when it is not small, the pool is the bottleneck rather than the database.",
			Watch: "Anything durably above zero means clients are queueing. The cause is either a pool_size too small for the concurrency, or servers held too long by slow queries — the maximum wait and the average query time tell you which. Adding application replicas makes a queue like this worse, not better.",
		},
	},
	{
		Key: "maxwait", Title: "Longest client wait", Unit: "s", Group: "queue",
		Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pgbouncer_pools_maxwait"},
		Summary: "How long the oldest client in the queue has been waiting.",
		Note: Note{
			Means: "The wait of the first client in the queue — the one that has been there longest. The console keeps the worst pool's figure.",
			Why:   "PgBouncer's own documentation names this the number to watch: if it starts rising, the pool of servers is not handling requests quickly enough.",
			Watch: "It should be zero nearly always, with brief spikes under bursts. A rising floor means either an overloaded PostgreSQL or a pool_size set too small. Sustained seconds here are seconds added to every query the application makes, and they will be blamed on the database rather than on the queue in front of it.",
		},
	},
	{
		Key: "cl-cancel-req", Title: "Cancellations queued", Unit: "requests", Group: "queue",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_cl_cancel_req"},
		Summary: "Query cancellations the pooler has not yet forwarded.",
		Note: Note{
			Means: "Cancel requests from clients that PgBouncer has accepted and not yet passed to a server.",
			Why:   "Cancellations queue behind the same shortage of server connections that queries do, so this rises for the same reason — and a cancellation that arrives late has usually stopped being useful.",
			Watch: "Normally zero. A build-up means clients are giving up on queries in numbers, which is a symptom of the wait above rather than a problem of its own.",
		},
	},

	// --------------------------------------------------- servers
	{
		Key: "sv-active", Title: "Server connections in use", Unit: "connections", Group: "servers",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_sv_active"},
		Summary: "Connections to PostgreSQL currently linked to a client.",
		Note: Note{
			Means: "Server connections carrying a client's work right now. Against pool_size, this is the pool's utilisation.",
			Why:   "It is the load the pooler is actually placing on PostgreSQL, which is the number the instance's own connection count should agree with.",
			Watch: "Sitting at pool_size with clients waiting is a pool at its ceiling. Well below pool_size with clients still waiting is stranger and worth chasing: it usually means the waits are in login or in server_check_query rather than in query execution.",
		},
	},
	{
		Key: "sv-idle", Title: "Server connections idle", Unit: "connections", Group: "servers",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_sv_idle"},
		Summary: "Connections open to PostgreSQL and immediately usable.",
		Note: Note{
			Means: "Server connections the pooler holds open, unused, and ready to hand to the next client without a login round trip.",
			Why:   "Idle connections are the pool's headroom. They are what makes a pooler faster than connecting directly, and they cost a backend slot on PostgreSQL for as long as they are held.",
			Watch: "Idle at zero while clients wait is the definition of an undersized pool. A large idle count that never falls is the opposite: connection slots reserved on the database for load that is not arriving.",
		},
	},
	{
		Key: "sv-used", Title: "Server connections needing a check", Unit: "connections", Group: "servers",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_sv_used"},
		Summary: "Connections idle longer than server_check_delay, awaiting their check query.",
		Note: Note{
			Means: "Connections that have been idle long enough that PgBouncer will run server_check_query on them before reusing them.",
			Why:   "They are usable, but not free: the next client to take one pays for the check first.",
			Watch: "A steady population here is normal on a quiet pool. It matters only when the check itself is slow, which turns every hand-out into a round trip and shows up as wait time with no queries to blame.",
		},
	},
	{
		Key: "sv-login", Title: "Server connections logging in", Unit: "connections", Group: "servers",
		Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_pools_sv_login"},
		Summary: "Connections to PostgreSQL part-way through authenticating.",
		Note: Note{
			Means: "Server connections currently opening: TCP established, login not finished.",
			Why:   "Logins are the expensive part of a connection, and the whole reason to pool. Seeing many at once means the pool is being rebuilt rather than reused.",
			Watch: "Brief spikes after a failover or a pooler restart are expected. A sustained population means connections are being churned — server_lifetime too short, PostgreSQL closing them, or the pool repeatedly emptying and refilling.",
		},
	},

	// ------------------------------------------------ throughput
	{
		Key: "queries", Title: "Queries pooled", Unit: "queries/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_query_count"},
		Summary: "Rate of SQL queries the pooler forwarded.",
		Note: Note{
			Means: "Queries PgBouncer carried, as a rate. Counted per database and summed here.",
			Why:   "It is the throughput the pooler is delivering, and the number to compare against the instance's own commit rate when deciding whether the two agree about how much work is arriving.",
			Watch: "A drop with clients still connected means work stopped arriving, not that the pooler failed. A drop with the waiting line rising means the opposite — work is arriving and not getting through.",
		},
	},
	{
		Key: "transactions", Title: "Transactions pooled", Unit: "tx/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_xact_count"},
		Summary: "Rate of SQL transactions the pooler forwarded.",
		Note: Note{
			Means: "Transactions carried, as a rate. In transaction pooling mode this is also the rate at which server connections are handed back to the pool.",
			Why:   "Queries divided by transactions gives the average statements per transaction, which is what decides how long each client holds a server connection in transaction mode.",
			Watch: "Long transactions are the enemy of transaction pooling: each one holds its server connection to the end. A falling transaction rate with a steady query rate means transactions are getting longer, and the pool will start queueing before any single query looks slow.",
		},
	},
	{
		Key: "query-time", Title: "Time spent in queries", Unit: "µs/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_query_time"},
		Summary: "Microseconds per second spent actively executing on PostgreSQL.",
		Note: Note{
			Means: "Time server connections spent running queries, in microseconds per second of real time. Divided by the query rate it is the average query duration.",
			Why:   "It is the database's half of the round trip, measured at the pooler — so it can be compared with the wait time beside it, which is the queue's half.",
			Watch: "Query time rising while wait time stays flat is PostgreSQL getting slower. Both rising together is the usual cascade: slower queries hold server connections longer, which lengthens the queue, which lengthens every client's wait.",
		},
	},
	{
		Key: "wait-time", Title: "Time spent waiting for a server", Unit: "µs/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_wait_time"},
		Summary: "Microseconds per second clients spent queued for a server connection.",
		Note: Note{
			Means: "Total time clients spent waiting, in microseconds per second. This is time added by the pool, before PostgreSQL sees the query at all.",
			Why:   "It separates the two halves of a slow request. An application timing its own queries sees this and the query time added together, and will blame the database for both.",
			Watch: "It should be a small fraction of the query time. When it is comparable or larger, the pool is the bottleneck and tuning PostgreSQL will not help — pool_size, or the transactions holding connections, is where the fix is.",
		},
	},
	{
		Key: "bytes-received", Title: "Traffic from clients", Unit: "bytes/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_received"},
		Summary: "Bytes per second the pooler read from clients.",
		Note: Note{
			Means: "Network volume arriving from the application side.",
			Why:   "Large query text, large parameter arrays and COPY traffic all show here and nowhere else on this screen.",
			Watch: "A rise with no rise in the query rate means the statements themselves got bigger — often an ORM sending a large IN list, or a switch to bulk COPY.",
		},
	},
	{
		Key: "bytes-sent", Title: "Traffic to clients", Unit: "bytes/s", Group: "throughput",
		Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_stats_total_sent"},
		Summary: "Bytes per second the pooler wrote to clients.",
		Note: Note{
			Means: "Network volume returning to the application side: result sets.",
			Why:   "It is the closest thing to a result-size measure the pooler has.",
			Watch: "Sent climbing far faster than the query rate means queries are returning more rows than they used to — a missing LIMIT, or a query whose selectivity changed with the data.",
		},
	},
}

var poolerInstants = []InstantDef{
	{
		Key: "pools", Title: "Pools", Unit: "pools", Group: "capacity",
		Render: RenderNumber, Aggregate: Max, Names: []string{"cnpg_pgbouncer_lists_pools"},
		Summary: "Distinct database-and-user pools this pooler maintains.",
		Note: Note{
			Means: "PgBouncer keeps one pool per (database, user) pair. This counts them.",
			Why:   "Every pool has its own pool_size, so the total connections a pooler may open to PostgreSQL is this multiplied by that — not pool_size alone.",
			Watch: "A count that grows with the number of application users is the usual way a pooler ends up opening far more backends than anyone intended.",
		},
	},
	{
		Key: "databases", Title: "Databases", Unit: "databases", Group: "capacity",
		Render: RenderNumber, Aggregate: Max, Names: []string{"cnpg_pgbouncer_lists_databases"},
		Summary: "Databases configured on this pooler.",
		Note: Note{
			Means: "Entries in PgBouncer's database list, including its own admin database.",
			Why:   "It says what this pooler is willing to route to, which is a configuration fact rather than a load one.",
			Watch: "Mostly static. A change here means the pooler's configuration changed.",
		},
	},
	{
		Key: "users", Title: "Users", Unit: "users", Group: "capacity",
		Render: RenderNumber, Aggregate: Max, Names: []string{"cnpg_pgbouncer_lists_users"},
		Summary: "Users known to this pooler.",
		Note: Note{
			Means: "Entries in PgBouncer's user list.",
			Why:   "With the database count it bounds how many pools can exist.",
			Watch: "Static unless the pooler's authentication configuration changes.",
		},
	},
	{
		Key: "used-clients", Title: "Client slots in use", Unit: "slots", Group: "capacity",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_lists_used_clients"},
		Summary: "Client connection slots currently occupied.",
		Note: Note{
			Means: "Slots PgBouncer allocated for client connections that are in use, counted across the whole process rather than per pool.",
			Why:   "Read against the free slots beside it, it is how close the pooler is to max_client_conn — the ceiling at which it stops accepting clients at all.",
			Watch: "Reaching the ceiling is a hard failure for the application: new connections are refused by the pooler, before PostgreSQL is involved. The free count is the headroom left.",
		},
	},
	{
		Key: "free-clients", Title: "Client slots free", Unit: "slots", Group: "capacity",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_lists_free_clients"},
		Summary: "Client connection slots still available.",
		Note: Note{
			Means: "Allocated client slots not currently in use.",
			Why:   "It is the headroom under max_client_conn, in the units the pooler itself counts.",
			Watch: "Falling towards zero means the pooler is about to start refusing connections. That failure looks to an application exactly like the database being down, and it is not.",
		},
	},
	{
		Key: "used-servers", Title: "Server slots in use", Unit: "slots", Group: "capacity",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_lists_used_servers"},
		Summary: "Server connection slots currently occupied.",
		Note: Note{
			Means: "Slots allocated for connections to PostgreSQL that are in use.",
			Why:   "It is the pooler's side of the count the instance reports as its own connections; the two should broadly agree.",
			Watch: "A pooler holding far more server connections than the instance reports backends, or the reverse, means one of the two is not seeing what the other is.",
		},
	},
	{
		Key: "login-clients", Title: "Clients logging in", Unit: "clients", Group: "capacity",
		Render: RenderNumber, Aggregate: Sum, Names: []string{"cnpg_pgbouncer_lists_login_clients"},
		Summary: "Client connections part-way through authenticating.",
		Note: Note{
			Means: "Clients that have connected to the pooler and not finished logging in.",
			Why:   "A population here is clients arriving faster than the pooler can admit them.",
			Watch: "Normally zero or brief. A standing count points at authentication being slow — an auth_query against a loaded database is the usual cause, and it delays every new connection.",
		},
	},
	{
		Key: "collection-error", Title: "Last collection", Group: "capacity",
		Render: RenderWords, Words: [2]string{"succeeded", "failed"}, Aggregate: Max,
		Names:   []string{"cnpg_pgbouncer_last_collection_error"},
		Summary: "Whether the exporter's last read of PgBouncer worked.",
		Note: Note{
			Means: "The exporter's own report of its last attempt to query PgBouncer's admin console.",
			Why:   "It separates 'the pooler is idle' from 'the numbers are stale'. If this failed, everything else on this screen is the last thing the exporter managed to read.",
			Watch: "A failure while the pod is Running means PgBouncer is not answering its admin interface. Treat every other reading here as suspect until it clears.",
		},
	},
}
