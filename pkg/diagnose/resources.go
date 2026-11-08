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

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// The conditions here read the operator's secondary resources — Pooler,
// FailoverQuorum, ImageCatalog — each a status the operator reports and
// the console already collects. Like every condition, they restate; the
// numbers quoted are the operator's own.

// PoolerShort matches a Pooler with fewer ready instances than declared.
type PoolerShort struct{}

func (PoolerShort) describe() string {
	return "a Pooler with fewer ready instances than declared"
}

func (PoolerShort) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := poolersUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, pooler := range in.Poolers.Poolers {
		if pooler.DesiredInstances == nil || pooler.ReadyInstances >= *pooler.DesiredInstances {
			continue
		}
		detail := fmt.Sprintf("%d instances declared, %d ready", *pooler.DesiredInstances, pooler.ReadyInstances)
		if pooler.Phase != "" {
			detail += ", phase " + pooler.Phase
			if pooler.PhaseReason != "" {
				detail += ": " + pooler.PhaseReason
			}
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + pooler.Name,
			subject:  EntityRef{Kind: "Pooler", Name: pooler.Name},
			summary: fmt.Sprintf("Pooler %s has %d of %d instances ready.",
				pooler.Name, pooler.ReadyInstances, *pooler.DesiredInstances),
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "Pooler/" + pooler.Name,
				Detail: detail,
			}},
			link:      "/poolers",
			linkLabel: "Poolers",
		})
	}
	return matches, ""
}

// QuorumStandbysShort matches a failover quorum whose potentially
// synchronous standbys number fewer than the standbys a transaction
// waits for. Absence of the resource is clear: the operator creates it
// only for a cluster running quorum-based synchronous replication.
type QuorumStandbysShort struct{}

func (QuorumStandbysShort) describe() string {
	return "a failover quorum with fewer potentially synchronous standbys than transactions wait for"
}

func (QuorumStandbysShort) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := quorumUnavailable(in); reason != "" {
		return nil, reason
	}
	quorum := in.FailoverQuorum.Quorum
	if !quorum.Present || quorum.StandbyNumber <= 0 {
		return nil, ""
	}
	if quorum.StandbysTruncated {
		return nil, "more standbys were reported than the console retains, so the count cannot be compared"
	}
	if len(quorum.Standbys) >= quorum.StandbyNumber {
		return nil, ""
	}
	detail := fmt.Sprintf("standbyNumber %d, standbyNames [%s]", quorum.StandbyNumber,
		strings.Join(quorum.Standbys, ", "))
	if quorum.Method != "" {
		detail += ", method " + quorum.Method
	}
	if quorum.Primary != "" {
		detail += ", written by " + quorum.Primary
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: fmt.Sprintf("Transactions wait for %d synchronous standbys and only %d are potentially synchronous.",
			quorum.StandbyNumber, len(quorum.Standbys)),
		evidence: []Evidence{{
			Origin: "operator-reported",
			Object: "FailoverQuorum",
			Detail: detail,
		}},
		link:      "/cluster/overview",
		linkLabel: "Overview",
	}}, ""
}

// catalogLookup is the outcome of resolving the Cluster's
// imageCatalogRef against the observed catalogs.
type catalogLookup int

const (
	// catalogFound: the referenced catalog was read.
	catalogFound catalogLookup = iota
	// catalogAbsent: the API server confirmed there is no such catalog.
	catalogAbsent
	// catalogUnreferenced: the Cluster names no catalog.
	catalogUnreferenced
)

// resolveCatalog follows the Cluster's imageCatalogRef to the observed
// catalog it names, or says why it cannot. Every way the lookup can
// decline is named: a cluster-scoped lookup the deployment did not opt
// into, one the console could not make, a namespaced set cut by the
// retention bound.
func resolveCatalog(in Input) (observe.ImageCatalogFacts, catalogLookup, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return observe.ImageCatalogFacts{}, catalogUnreferenced, reason
	}
	ref := in.Cluster.Cluster.ImageCatalogRef
	if ref == nil {
		return observe.ImageCatalogFacts{}, catalogUnreferenced, ""
	}
	if reason := imageCatalogsUnavailable(in); reason != "" {
		return observe.ImageCatalogFacts{}, catalogUnreferenced, reason
	}
	snapshot := in.ImageCatalogs
	switch ref.Kind {
	case "ImageCatalog":
		if catalog, ok := snapshot.Catalog(ref.Name); ok {
			return catalog, catalogFound, ""
		}
		if snapshot.Truncated {
			return observe.ImageCatalogFacts{}, catalogUnreferenced,
				"more catalogs exist than the console retains, so the referenced one may be among those not shown"
		}
		return observe.ImageCatalogFacts{}, catalogAbsent, ""
	case "ClusterImageCatalog":
		switch snapshot.ClusterCatalogState {
		case observe.ClusterCatalogPresent:
			return snapshot.ClusterCatalog, catalogFound, ""
		case observe.ClusterCatalogAbsent:
			return observe.ImageCatalogFacts{}, catalogAbsent, ""
		case observe.ClusterCatalogDisabled:
			return observe.ImageCatalogFacts{}, catalogUnreferenced,
				"the cluster-scoped catalog lookup is not enabled for this deployment"
		default:
			return observe.ImageCatalogFacts{}, catalogUnreferenced,
				"the cluster-scoped catalog could not be looked up"
		}
	}
	return observe.ImageCatalogFacts{}, catalogUnreferenced, ""
}

// ImageCatalogMissing matches a Cluster whose imageCatalogRef names a
// catalog the API server confirms does not exist.
type ImageCatalogMissing struct{}

func (ImageCatalogMissing) describe() string {
	return "a Cluster whose imageCatalogRef names a catalog that does not exist"
}

func (ImageCatalogMissing) evaluate(_ string, in Input) ([]conditionMatch, string) {
	_, lookup, reason := resolveCatalog(in)
	if reason != "" {
		return nil, reason
	}
	if lookup != catalogAbsent {
		return nil, ""
	}
	ref := in.Cluster.Cluster.ImageCatalogRef
	return []conditionMatch{{
		subject: clusterSubject,
		evidence: []Evidence{
			{
				Origin: "operator-reported", Object: "Cluster",
				Detail: fmt.Sprintf("imageCatalogRef %s/%s", ref.Kind, ref.Name),
			},
			{
				Origin: "Kubernetes-observed", Object: ref.Kind + "/" + ref.Name,
				Detail: "the API server reports no such object",
			},
		},
		link:      "/objects",
		linkLabel: "Objects",
	}}, ""
}

// ImageCatalogLacksMajor matches a referenced catalog that offers no
// image for the PostgreSQL major the Cluster runs.
type ImageCatalogLacksMajor struct{}

func (ImageCatalogLacksMajor) describe() string {
	return "a referenced image catalog with no image for the Cluster's PostgreSQL major"
}

func (ImageCatalogLacksMajor) evaluate(_ string, in Input) ([]conditionMatch, string) {
	catalog, lookup, reason := resolveCatalog(in)
	if reason != "" {
		return nil, reason
	}
	if lookup != catalogFound {
		return nil, ""
	}
	major := in.Cluster.Cluster.PostgresMajorVersion
	if major == nil {
		return nil, "the operator reports no PostgreSQL major version to look up"
	}
	majors := make([]string, 0, len(catalog.Images))
	for _, image := range catalog.Images {
		if image.Major == *major {
			return nil, ""
		}
		majors = append(majors, strconv.Itoa(image.Major))
	}
	if catalog.ImagesTruncated {
		return nil, "the catalog offers more images than the console retains, so the major may be among those not shown"
	}
	ref := in.Cluster.Cluster.ImageCatalogRef
	return []conditionMatch{{
		subject: EntityRef{Kind: ref.Kind, Name: ref.Name},
		summary: fmt.Sprintf("The referenced %s offers no image for PostgreSQL %d.", ref.Kind, *major),
		evidence: []Evidence{
			{
				Origin: "operator-reported", Object: "Cluster",
				Detail: fmt.Sprintf("imageCatalogRef %s/%s, PostgreSQL major version %d", ref.Kind, ref.Name, *major),
			},
			{
				Origin: "Kubernetes-observed", Object: ref.Kind + "/" + ref.Name,
				Detail: fmt.Sprintf("%d images, for majors [%s]", len(catalog.Images), strings.Join(majors, ", ")),
			},
		},
		link:      "/objects",
		linkLabel: "Objects",
	}}, ""
}
