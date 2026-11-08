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

// noBackupAfter is how old a schedule must be before the absence of any
// successful backup is a finding: a day, which every schedule that
// fires at all has fired within.
const noBackupAfter = 24 * time.Hour

// barmanRules are the claims about backup and archiving through the
// Barman Cloud tooling — messages the barman-cloud commands themselves
// write, whichever process invokes them. They are unpinned
// deliberately: each message has held stable across the releases the
// console has been tested with, and the plugin sidecar's observed version
// (parsed from its image tag) is there to pin against the day one of
// them is reworded.
func barmanRules() []diagnose.Rule {
	const bestEffort = "Read from the container's log while following it. Following is " +
		"best effort, so the count below is a floor and an absence here " +
		"rules nothing out."
	return []diagnose.Rule{
		{
			ID:        "wal-archive-not-empty",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the archiver refusing a WAL archive that is not empty",
			Summary: "The configured WAL archive is not empty, so the operator " +
				"refused to start archiving into it.",
			Detail: bestEffort,
			When:   diagnose.LogContains{Substrings: []string{"WAL archive check failed"}},
			NextSteps: "Point the store at an empty destination or a distinct serverName. " +
				"Reusing an old server folder on purpose means emptying it first — " +
				"the refusal exists so one cluster cannot silently overwrite " +
				"another's recovery data.",
		},
		{
			ID:        "backup-destination-conflict",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "a backup destination already holding another cluster's data",
			Summary: "The backup destination already holds data for this server " +
				"name, so the operator refused to write into it.",
			Detail: bestEffort,
			When:   diagnose.LogContains{Substrings: []string{"backup", "already exists"}},
		},
		{
			ID:        "object-store-denied",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the object store refusing the configured credentials",
			Summary: "The object store refused the operator's credentials for the " +
				"configured destination.",
			Detail: bestEffort,
			When:   diagnose.LogContains{Substrings: []string{"AccessDenied"}},
			NextSteps: "Fix the store's credentials or the bucket policy they run into; " +
				"archiving resumes on its own once a write succeeds.",
		},
		{
			// The same refusal in the other dialect. AWS answers bad
			// credentials with an AccessDenied code; MinIO answers the
			// same request with a bare 403 — observed live as "An error
			// occurred (403) when calling the HeadBucket operation:
			// Forbidden" — and one substring cannot cover both.
			ID:        "object-store-forbidden",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the object store answering 403 Forbidden",
			Summary: "The object store answered 403 Forbidden, so the configured " +
				"credentials do not grant this destination.",
			Detail: bestEffort,
			When:   diagnose.LogContains{Substrings: []string{"An error occurred (403)"}},
			NextSteps: "Fix the store's credentials or the bucket policy they run into; " +
				"archiving resumes on its own once a write succeeds.",
		},
		{
			ID:        "object-store-unreachable",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "an unreachable object store endpoint",
			Summary: "The operator could not reach the configured object store " +
				"endpoint.",
			Detail: bestEffort,
			When:   diagnose.LogContains{Substrings: []string{"could not connect to", "endpoint"}},
			NextSteps: "Check the endpoint URL, DNS from inside the namespace, and any " +
				"NetworkPolicy between the instance pods and the store.",
		},
		{
			// The two rules below read the plugin's own summary of the
			// store rather than a log line, and so are the plugin's
			// account rather than the archiver's. Unpinned like the rest
			// of this file: the ObjectStore status is the plugin's API,
			// declared in its own module rather than the operator's.
			ID:        "barman-last-backup-failed",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityWarning,
			Describes: "the barman-cloud plugin reporting the last backup as failed",
			Summary:   "The barman-cloud plugin reports this cluster's most recent backup as failed.",
			Detail: "The plugin summarises each server's backups in its ObjectStore " +
				"status. A failure more recent than the last success means the " +
				"recovery window has stopped growing; the Backup object and its " +
				"events carry the error.",
			When:      diagnose.StoreBackupFailed{},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "barman-no-successful-backup",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityHigh,
			Describes: "the barman-cloud plugin reporting no successful backup for a cluster scheduled for a day",
			Summary:   "The object store holds no successful backup of this cluster, though a backup schedule has existed for a day.",
			Detail: "A schedule a day old has had its first run, and the plugin " +
				"reports no successful backup for this cluster's server. There is " +
				"no recovery point: nothing in the store can restore this cluster.",
			When: diagnose.StoreNoBackup{Since: noBackupAfter},
			NextSteps: "Look at the Backup objects the schedule created and their " +
				"events: each failed one quotes why. Until one succeeds, the " +
				"cluster has no backup at all.",
			Link:      "/backups",
			LinkLabel: "Backups",
		},
	}
}
