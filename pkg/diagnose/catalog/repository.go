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

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// repositoryRules restate what the evidence agent reports
// about the backup repository (barman-cloud or pgBackRest, through the
// format-neutral facts): typed states with stable reason
// codes, nothing more. They are the only rules that read the
// repository's side of the backup story, and they keep the console's
// standing rule about it — an observation is placed beside the
// operator's claim, never merged into it, and no finding here says what
// the repository would or would not restore.
//
// They are unpinned: the evidence agent's API is versioned by its own contract,
// which the evidence consumer enforces before a report reaches the
// detectors. A report of an unrecognised variant reads as could-not-run.
func repositoryRules() []diagnose.Rule {
	const restated = "The state and reason code are the evidence agent's own, restated; the " +
		"evidence agent's diagnostic text stays in the evidence agent. Open the evidence " +
		"screen for the full report."
	return []diagnose.Rule{
		{
			ID:        "repository-wal-unhealthy",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the evidence agent reporting WAL continuity as unhealthy",
			Summary:   "The evidence agent reports the archived WAL sequence as unhealthy.",
			Detail: "The evidence agent walked the repository's WAL objects and found the " +
				"sequence in a state it classes as unhealthy — the reason code names " +
				"which. An archive with a break in it bounds how far recovery can " +
				"replay, whatever the operator's archiving condition says now. " + restated,
			When: diagnose.RepositoryState{Aspect: diagnose.RepositoryWAL},
			// The operator's condition explains a gap that is still being
			// created; a gap from an earlier outage stands on its own.
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}},
			Link:          "/backups/evidence",
			LinkLabel:     "Repository evidence",
		},
		{
			ID:        "repository-coverage-unhealthy",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the evidence agent reporting recovery coverage as unhealthy",
			Summary:   "The evidence agent reports the observed recovery coverage as unhealthy.",
			Detail: "Coverage is the evidence agent's traversal from each backup through the " +
				"WAL that follows it; unhealthy means it found no path it classes as " +
				"usable from the backups it examined. " + restated,
			When: diagnose.RepositoryState{Aspect: diagnose.RepositoryCoverage},
			ConsequenceOf: []diagnose.Relation{
				{Cause: "repository-wal-unhealthy"}, {Cause: "cnpg-wal-archiving-failing"},
			},
			Link:      "/backups/evidence",
			LinkLabel: "Repository evidence",
		},
		{
			ID:        "repository-retention-unhealthy",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityWarning,
			Describes: "the evidence agent reporting retention as unhealthy",
			Summary:   "The evidence agent reports retention as unhealthy against the configured expectation.",
			Detail: "The evidence agent compares the backups it can see against the retention " +
				"it was given: the minimum redundancy the operator configured (Barman), " +
				"or the plugin's repo-retention-* options (pgBackRest). Unhealthy means " +
				"the repository holds fewer usable backups than expected, or (pgBackRest, " +
				"finding RetentionShortfall) lacks a full backup or WAL the retention " +
				"still keeps. " + restated,
			When:      diagnose.RepositoryState{Aspect: diagnose.RepositoryRetention},
			Link:      "/backups/evidence",
			LinkLabel: "Repository evidence",
		},
		{
			// pgBackRest expires only at the end of a backup: a stanza that
			// still holds what its retention expires has no expire running.
			ID:        "repository-retention-not-applied",
			Component: diagnose.ComponentPgBackRest,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityWarning,
			Describes: "the evidence agent raising the RetentionNotApplied finding",
			Summary:   "The pgBackRest stanza holds backups or WAL its retention would have expired.",
			Detail: "pgBackRest expires at the end of each backup. The stanza still holds a " +
				"backup or WAL older than what the plugin's repo-retention-* options keep " +
				"as of the newest backup, so expire did not run or ran with another " +
				"retention: the repository grows past what the retention promises. " +
				"Check that backups complete and that the plugin passes the retention " +
				"the Cluster declares. " + restated,
			When:      diagnose.RepositoryFinding{Code: "RetentionNotApplied"},
			Link:      "/backups/evidence",
			LinkLabel: "Repository evidence",
		},
		{
			ID:        "repository-retention-not-configured",
			Component: diagnose.ComponentPgBackRest,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityNote,
			Describes: "the evidence agent raising the RetentionNotConfigured finding",
			Summary:   "No full backup retention is set for the pgBackRest repository: nothing is ever expired.",
			Detail: "repo-retention-full is unset, so pgBackRest keeps every full backup and " +
				"the WAL from the oldest one: the repository grows without bound. Set a " +
				"retention on the plugin's repository if that is not intended. " + restated,
			When:      diagnose.RepositoryFinding{Code: "RetentionNotConfigured"},
			Link:      "/backups/evidence",
			LinkLabel: "Repository evidence",
		},
		{
			// Opera's configuration.stanza can send WAL to
			// another stanza than the backups.
			ID:        "repository-stanza-split",
			Component: diagnose.ComponentPgBackRest,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityWarning,
			Describes: "the evidence agent raising the StanzaSplit finding",
			Summary:   "The pgBackRest backups and their WAL sit under two different stanzas.",
			Detail: "The plugin writes the backups under one stanza and archives the WAL " +
				"under another (configuration.stanza of the Archive). pgBackRest looks for " +
				"a backup's WAL under the stanza it restores from, so a restore that names " +
				"only the backup stanza finds none of it: name the archive stanza for the " +
				"WAL, or align the two. " + restated,
			When:      diagnose.RepositoryFinding{Code: "StanzaSplit"},
			Link:      "/backups/evidence",
			LinkLabel: "Repository evidence",
		},
	}
}
