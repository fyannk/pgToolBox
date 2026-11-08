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

package catalog

import (
	"time"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The PostgreSQL thresholds, console-pinned knowledge stated in one
// place. Each rule's check row renders the number it applies, so a
// reader never has to trust this declaration to know what was compared.
const (
	// xidWraparoundAge is the transaction-id age that counts as
	// approaching wraparound. Ids wrap at two billion usable values and
	// PostgreSQL refuses writes with three million left; 1.6 billion
	// leaves under a quarter of the headroom.
	xidWraparoundAge = 1_600_000_000
	// longTransactionSeconds is how long one transaction must have been
	// open. An hour is far past any interactive statement and is where
	// a held horizon starts costing vacuum real work.
	longTransactionSeconds = 3600
	// longTransactionHeld is how long that age must hold. A report that
	// finishes inside the window was work, not a leak.
	longTransactionHeld = 30 * time.Minute
	// deadlockRate is the rate of deadlocks, per second, that counts as
	// continuous: one a minute. A deadlock is PostgreSQL killing one of
	// two transactions so the other can finish; one a minute for a
	// quarter of an hour is an application pattern, not an accident.
	deadlockRate = 1.0 / 60
	// deadlockHeld is how long that rate must hold.
	deadlockHeld = 15 * time.Minute
	// backendsWaiting is how many backends may wait on a lock at once.
	// It is the number the operator's own sample alerting rule uses.
	backendsWaiting = 300
	// backendsWaitingHeld is how long that many must wait. The
	// operator's sample rule holds for a minute; five is past the
	// longest lock a migration takes in ordinary operation.
	backendsWaitingHeld = 5 * time.Minute
)

// postgresRules are the claims about PostgreSQL itself.
func postgresRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			// The severity is read from the field CloudNativePG's log pipe
			// puts it in, which is why a PostgreSQL rule carries a
			// CloudNativePG pin: the message is the database's, the
			// envelope is the operator's. Reading the field rather than
			// searching the line keeps a severity quoted inside some
			// other component's message from counting as the server
			// speaking, and it survives any reordering of the envelope's
			// keys. The path is the one the operator's own log decoder
			// declares, unchanged across the verified releases.
			ID:        "postgres-fatal",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Severity:  diagnose.SeverityWarning,
			Describes: "a server log record with FATAL severity",
			Summary:   "PostgreSQL logged a FATAL-severity record.",
			Detail: "FATAL ends one backend, not the server, and routine failures — a " +
				"wrong password, a refused connection during startup — log at this " +
				"severity too; the quoted record says which this is. Read from the " +
				"container's log while following it, best effort: the count below is " +
				"a floor and an absence here rules nothing out.",
			When: diagnose.LogFields{
				Fields: []diagnose.LogField{
					{Path: "record.error_severity", Equals: "FATAL"},
				},
				// PostgreSQL stamps routine lifecycle refusals FATAL too,
				// and the operator's own probes provoke them on every
				// start: a connection during startup, shutdown, or crash
				// recovery is refused at this severity, and an orderly
				// switchover terminates backends with it. Reporting those
				// would fire on every restart of every cluster forever,
				// which is how a screen teaches people to ignore it. The
				// messages are verbatim server strings, stable across the
				// pinned majors.
				Except: []string{
					"the database system is starting up",
					"the database system is not yet accepting connections",
					"the database system is shutting down",
					"the database system is in recovery mode",
					"terminating connection due to administrator command",
				},
			},
		},
		{
			ID:        "postgres-panic",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Severity:  diagnose.SeverityCritical,
			Describes: "a server log record with PANIC severity",
			Summary:   "PostgreSQL panicked, which ends the whole server process.",
			Detail: "A panic crashes the postmaster and forces crash recovery. Read from " +
				"the container's log while following it, best effort: the count below " +
				"is a floor and an absence here rules nothing out.",
			When: diagnose.LogFields{Fields: []diagnose.LogField{
				{Path: "record.error_severity", Equals: "PANIC"},
			}},
			// The panic is the server on one instance failing to write WAL, so
			// the relation is pinned to the pod and to the hour: both lines
			// carry a time, and a disk-full line hours old on another
			// instance explains nothing here.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-wal-disk-full", Scope: diagnose.ScopePod, Within: time.Hour},
			},
		},
		{
			// The threshold is pinned knowledge, like the EOL boundary:
			// transaction ids wrap at two billion usable values, and
			// PostgreSQL stops accepting writes with three million left.
			// At 1.6 billion the cluster still works, which is exactly
			// why the number deserves a finding — nothing else looks
			// wrong while the headroom runs out. The metric name is the
			// CloudNativePG exporter's, hence the operator pin.
			ID:        "postgres-xid-wraparound",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Pinned:    []string{"xid_age"},
			Severity:  diagnose.SeverityCritical,
			Describes: "a database's transaction-id age near wraparound",
			Summary:   "A database's transaction-id age is approaching wraparound, which ends in PostgreSQL refusing writes.",
			Detail: "Something is holding the oldest transaction horizon — a forgotten " +
				"open transaction, a stale replication slot, a failing autovacuum — " +
				"and the age below leaves under a quarter of the headroom. If it " +
				"reaches the hard limit, PostgreSQL stops accepting writes " +
				"cluster-wide until a vacuum completes. The boundary is " +
				"console-pinned knowledge; the reading is the exporter's.",
			When: diagnose.SeriesAbove{Key: "xid-age", Threshold: xidWraparoundAge},
			// The horizon is held by something, and a transaction left
			// open is the most common something. The two readings can
			// come from different instances, so the relation is
			// cluster-wide.
			ConsequenceOf: []diagnose.Relation{{Cause: "postgres-long-transaction"}},
		},
		{
			ID:        "postgres-mxid-wraparound",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Pinned:    []string{"mxid_age"},
			Severity:  diagnose.SeverityCritical,
			Describes: "a database's multixact-id age near wraparound",
			Summary:   "A database's multixact-id age is approaching wraparound, which ends in PostgreSQL refusing writes.",
			Detail: "The multixact counter wraps exactly like the transaction counter " +
				"and is exhausted by the same causes, plus heavy row-level sharing. " +
				"The same consequence applies: past the hard limit, writes stop " +
				"until vacuum catches up.",
			When:          diagnose.SeriesAbove{Key: "mxid-age", Threshold: xidWraparoundAge},
			ConsequenceOf: []diagnose.Relation{{Cause: "postgres-long-transaction"}},
		},
		{
			// The version is the finding: there is no observation to wait
			// for. Where support ends is data, read from the lifecycle
			// table (built in, or the deployment's ConfigMap) and compared
			// with the run's clock, so the boundary moves by itself as
			// dates pass and needs no release.
			ID:        "postgres-eol",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityWarning,
			Describes: "a PostgreSQL major version past upstream end of life",
			Summary:   "The PostgreSQL major version no longer receives upstream releases.",
			Detail: "The PostgreSQL project supports each major for five years, so bug and " +
				"security fixes are no longer published for this one. Where support ends " +
				"is the lifecycle table's claim, quoted below with the version it is applied to.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentPostgreSQL},
			Link:      "/",
			LinkLabel: "Overview",
		},
		{
			ID:        "postgres-eol-soon",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityNote,
			Describes: "a PostgreSQL major version leaving upstream support within 180 days",
			Summary:   "The PostgreSQL major version leaves upstream support soon.",
			Detail: "A major upgrade takes planning; the date below, from the lifecycle " +
				"table, is when fixes stop being published for this one.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentPostgreSQL, Within: 180 * 24 * time.Hour},
			Link:      "/",
			LinkLabel: "Overview",
		},
		{
			// The cause the two wraparound rules point back to. On its
			// own it is a warning: a transaction open for an hour is
			// often a long report, and the console cannot tell that from
			// a forgotten session. What it no longer is, is invisible.
			ID:        "postgres-long-transaction",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Pinned:    []string{"max_tx_duration_seconds"},
			Severity:  diagnose.SeverityWarning,
			Describes: "an instance with a transaction open past the threshold",
			Summary:   "A transaction has been open past the threshold across the retained window.",
			Detail: "An open transaction holds the oldest transaction horizon, and vacuum " +
				"cannot clean any row version newer than what that horizon protects. " +
				"Dead rows accumulate and transaction-id age climbs for as long as it " +
				"is held. A long analytics query looks exactly like a forgotten " +
				"session here; which of the two this is, the console cannot tell.",
			When: diagnose.SeriesAbove{
				Key: "max-tx-duration", Threshold: longTransactionSeconds, For: longTransactionHeld,
			},
			NextSteps: "Find the backend on the instance and decide whether it is work or a " +
				"leak. An idle-in-transaction session is the usual leak, and it holds " +
				"the horizon just as firmly as a running query does.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			// PostgreSQL counts deadlocks per database; the exporter's
			// default query publishes the count, and the console reads it
			// as a rate.
			ID:        "postgres-deadlocks-ongoing",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Severity:  diagnose.SeverityWarning,
			Describes: "deadlocks at a rate of one a minute or more, held for a quarter of an hour",
			Summary:   "PostgreSQL has been breaking deadlocks continuously for a quarter of an hour.",
			Detail: "Every deadlock is one transaction killed so another can finish. A " +
				"rate that holds is two code paths taking locks in opposite orders, " +
				"again and again; the server log names the statements each time.",
			When:      diagnose.SeriesAbove{Key: "deadlocks", Threshold: deadlockRate, For: deadlockHeld},
			Pinned:    []string{"pg_stat_database", "deadlocks"},
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			ID:        "postgres-backends-waiting",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Severity:  diagnose.SeverityHigh,
			Describes: "at least 300 backends waiting on a lock, held for five minutes",
			Summary:   "Hundreds of backends have been waiting on locks for five minutes: something holds a lock everything else needs.",
			Detail: "Backends waiting are sessions blocked behind a lock another " +
				"session holds — a long transaction, a migration, a vacuum that " +
				"cannot finish. The lock holder is the finding; the waiters are " +
				"its symptom.",
			When:          diagnose.SeriesAbove{Key: "backends-waiting", Threshold: backendsWaiting, For: backendsWaitingHeld},
			Pinned:        []string{"backends_waiting"},
			ConsequenceOf: []diagnose.Relation{{Cause: "postgres-long-transaction", Strength: diagnose.StrengthPlausible}},
			Link:          "/cluster/metrics",
			LinkLabel:     "Metrics",
		},
		{
			// A note, not a fault: the exporter's default query compares
			// each installed extension against the version the image ships.
			ID:        "postgres-extension-update-available",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Requires: []diagnose.Requirement{
				{Component: diagnose.ComponentCNPG, Constraint: ">=1.30 <1.31"},
			},
			Severity:  diagnose.SeverityNote,
			Describes: "an installed extension older than the version its image ships",
			Summary:   "An installed extension is older than the version the image ships, and has not been updated.",
			Detail: "A new image carries newer extension files, but the extension " +
				"installed in each database stays at its version until ALTER " +
				"EXTENSION UPDATE is run. Nothing breaks; the newer code is simply " +
				"not in use.",
			When:      diagnose.InstantNonZero{Key: "extensions-update"},
			Pinned:    []string{"pg_extensions", "update_available"},
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
	}
}
