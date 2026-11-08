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

package observe

import (
	"time"
)

// QuotaResourceFacts is one resource line of a ResourceQuota: the
// declared ceiling, the API server's reported usage, and whether the
// ceiling is reached — computed at the observation boundary, where the
// quantities can still be compared as quantities.
type QuotaResourceFacts struct {
	// Resource is the quota key, such as "requests.storage" or "pods".
	Resource string
	// Hard is the declared ceiling, and Used the reported usage, in the
	// quantity's canonical rendering — "1024Mi" reads back as "1Gi",
	// the same value in the form Kubernetes itself normalizes to.
	Hard string
	// Used is the reported usage; empty when unreported.
	Used string
	// Exhausted reports used >= hard. It is derived at conversion from
	// the two quantities above, which the reader can check against it.
	Exhausted bool
}

// QuotaFacts is the Kubernetes-reported state of one ResourceQuota.
type QuotaFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Resources are the quota's lines, sorted by resource name and
	// bounded by the source.
	Resources []QuotaResourceFacts
}

// QuotasSnapshot is the rendered quota set, immutable and carrying its
// own staleness.
type QuotasSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact.
	Stale bool
	// Quotas is sorted by name; the producer bounds it.
	Quotas []QuotaFacts
}
