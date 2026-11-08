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

package diagnose

// Layer is where in the stack a check looks: the answer to "which side
// is it" that an operator asks before "what is it". It is coarser than
// Component and cuts across it — a CloudNativePG rule about replication
// lag and a PostgreSQL rule about a long transaction are both about the
// database layer's health, while a CloudNativePG rule about a phase is
// about the operator's. Every check declares one, so the screen can
// fold the run into one line per layer without guessing from names.
type Layer string

const (
	// LayerKubernetes is scheduling, images, volumes, quotas, evictions
	// and the API server itself: what has to hold before the operator
	// can act.
	LayerKubernetes Layer = "Kubernetes"
	// LayerOperator is the operator's reconciliation: its phases,
	// conditions, plugins, catalogs, certificates and the objects it
	// declares into the cluster.
	LayerOperator Layer = "Operator"
	// LayerPostgreSQL is the database process on each instance: starts,
	// crashes, disk, wraparound, locks.
	LayerPostgreSQL Layer = "PostgreSQL"
	// LayerReplication is the relationship between instances: the
	// primary and who holds that role, streaming, lag, slots, quorum.
	LayerReplication Layer = "Replication"
	// LayerBackups is the path out of the cluster: WAL archiving,
	// backups, schedules, the object store and what it holds.
	LayerBackups Layer = "Backups and archive"
	// LayerPoolers is the connection poolers in front of the cluster.
	LayerPoolers Layer = "Poolers"
	// LayerDeclared is the database objects declared through the
	// operator: databases, roles, publications, subscriptions.
	LayerDeclared Layer = "Declared objects"
)

// Layers is the closed set, in the order the screen reads them: from
// the platform up through the operator to the database, then outward.
func Layers() []Layer {
	return []Layer{
		LayerKubernetes, LayerOperator, LayerPostgreSQL, LayerReplication,
		LayerBackups, LayerPoolers, LayerDeclared,
	}
}
