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

package cnpg

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// resourceRules are the claims about the operator's secondary resources:
// the Pooler, the FailoverQuorum, and the image catalogs a Cluster
// references. Each reads a status the operator reports on the resource
// itself, so the pins rest on the status fields' own JSON tags, which
// are what the console decodes.
func resourceRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-pooler-short",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Pinned:    []string{"PoolerStatus", `json:"readyInstances,omitempty"`},
			Severity:  diagnose.SeverityWarning,
			Describes: "a Pooler with fewer ready instances than declared",
			Summary:   "A Pooler has fewer ready instances than it declares.",
			Detail: "Applications reach PostgreSQL through the pooler's Service, so a " +
				"pooler short of members has less capacity than its size suggests, " +
				"and a pooler with none is an outage for everything routed through " +
				"it while the instances themselves read healthy. The pooler pods' " +
				"own container states, checked separately, usually say why.",
			When: diagnose.PoolerShort{},
			NextSteps: "Read the pooler pods' states and log tails: a crash-looping " +
				"pgbouncer names its reason there, and a Pending pod is the " +
				"scheduler's or the quota's to explain.",
			Link:      "/poolers",
			LinkLabel: "Poolers",
		},
		{
			// PgBouncer's own number to watch: the wait of the oldest
			// queued client. Five seconds is console-pinned knowledge —
			// long enough to outlast a burst, short enough that every
			// query the application makes is already being blamed on the
			// database by then.
			ID:        "cnpg-pooler-clients-waiting",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Pinned:    []string{"maxwait"},
			Severity:  diagnose.SeverityWarning,
			Describes: "a pooler instance whose oldest queued client has waited at least five seconds",
			Summary:   "Clients are queueing at a pooler: the oldest has waited at least five seconds for a server connection.",
			Detail: "The pool of server connections is not keeping up with the clients " +
				"in front of it. Every second here is added to every query the " +
				"application makes, and the delay is blamed on the database rather " +
				"than on the queue. The threshold is console-pinned knowledge; the " +
				"reading is the exporter's.",
			When: diagnose.SeriesAbove{Key: "maxwait", Threshold: 5, Pooler: true},
			NextSteps: "Compare the pooler's average query time with its wait: slow " +
				"queries holding servers need the query fixed, while fast queries " +
				"with a long queue need a larger pool_size or more server " +
				"connections on the instance. Adding application replicas makes " +
				"the queue longer, not shorter.",
			Link:      "/poolers/metrics",
			LinkLabel: "Pooler metrics",
		},
		{
			ID:        "cnpg-quorum-standbys-short",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Pinned:    []string{`json:"standbyNumber`, `json:"standbyNames`},
			Severity:  diagnose.SeverityCritical,
			Describes: "a failover quorum with fewer potentially synchronous standbys than transactions wait for",
			Summary:   "Fewer standbys are potentially synchronous than transactions wait for.",
			Detail: "The primary's instance manager reports how many synchronous standbys " +
				"a commit waits for and which instances could be one. When the " +
				"second number is smaller than the first, commits wait on standbys " +
				"that do not exist: writes stall until enough replicas stream again, " +
				"and a failover cannot elect a standby the quorum never saw.",
			When: diagnose.QuorumStandbysShort{},
			NextSteps: "Get the missing replicas streaming — the replication checks " +
				"and the pods' states say which are down — or lower the required " +
				"number in the cluster's synchronous configuration if the topology " +
				"has shrunk on purpose.",
			Link:      "/cluster/overview",
			LinkLabel: "Overview",
		},
		{
			ID:        "cnpg-image-catalog-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Pinned:    []string{"ImageCatalogRef", `json:"imageCatalogRef`},
			Severity:  diagnose.SeverityHigh,
			Describes: "a Cluster whose imageCatalogRef names a catalog that does not exist",
			Summary:   "The Cluster references an image catalog that does not exist.",
			Detail: "The operator resolves the container image from the catalog on " +
				"every reconcile. With no catalog to read, no image can be picked: " +
				"pods keep the image they run, and no upgrade or new instance can " +
				"start until the reference resolves.",
			When: diagnose.ImageCatalogMissing{},
			NextSteps: "Create the catalog the reference names, or point the reference " +
				"at one that exists. Check the kind as well as the name: a " +
				"namespaced ImageCatalog and a ClusterImageCatalog are different " +
				"objects.",
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-image-catalog-lacks-major",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Pinned:    []string{"ImageCatalogRef", `json:"imageCatalogRef`},
			Severity:  diagnose.SeverityHigh,
			Describes: "a referenced image catalog with no image for the Cluster's PostgreSQL major",
			Summary:   "The referenced image catalog offers no image for the PostgreSQL major the Cluster runs.",
			Detail: "A catalog lists one image per major. The Cluster's major is not " +
				"among the ones this catalog offers, so the operator has nothing to " +
				"resolve the image to — the same dead end as a missing catalog, one " +
				"entry narrower.",
			When: diagnose.ImageCatalogLacksMajor{},
			NextSteps: "Add an image for the Cluster's major to the catalog, or " +
				"reference a catalog that carries one.",
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			// The pooler phases are new in 1.30; before it a Pooler has no
			// phase to report.
			ID:        "cnpg-pooler-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a Pooler the operator reports as failed",
			Summary:   "A Pooler has failed: the operator cannot reconcile it, and quotes why.",
			Detail: "The failed phase is a configuration the operator cannot act on — " +
				"most often an image it cannot resolve from the catalog the Pooler " +
				"names. Clients using this pooler's service have nothing behind it " +
				"until the configuration is corrected.",
			When:          diagnose.PoolerPhase{AnyOf: []string{"failed"}},
			Pinned:        []string{"PoolerPhaseFailed"},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-pooler-image-catalog-error"}},
			Link:          "/poolers",
			LinkLabel:     "Poolers",
		},
		{
			ID:        "cnpg-pooler-inactive",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a Pooler the operator reports as inactive",
			Summary:   "A Pooler is inactive: something it needs does not exist yet.",
			Detail: "Inactive is the operator waiting on a prerequisite — the Cluster " +
				"the Pooler names, or the Secret it authenticates with. The quoted " +
				"reason names it.",
			When:      diagnose.PoolerPhase{AnyOf: []string{"inactive"}},
			Pinned:    []string{"PoolerPhaseInactive"},
			Link:      "/poolers",
			LinkLabel: "Poolers",
		},
		{
			// A note, like hibernation: a paused pooler is deliberate, and a
			// reader asking why the pooler answers nothing is told so.
			ID:        "cnpg-pooler-paused",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityNote,
			Describes: "a Pooler the operator reports as paused",
			Summary:   "A Pooler is paused on purpose: it runs no instances and answers no client.",
			When:      diagnose.PoolerPhase{AnyOf: []string{"paused"}},
			Pinned:    []string{"PoolerPhasePaused"},
			Link:      "/poolers",
			LinkLabel: "Poolers",
		},
	}
}
