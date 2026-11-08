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

// Image catalog bounds. A catalog carries at most eight images by the
// CRD's own validation; the ceilings here bound the namespace's catalogs
// and guard against a resource that predates that validation.
const (
	// MaxImageCatalogs bounds retained and rendered catalogs.
	MaxImageCatalogs = 32
	// MaxCatalogImages bounds the images rendered from one catalog.
	MaxCatalogImages = 16
)

// ClusterCatalogState is the outcome of the optional cluster-scoped
// catalog lookup. A ClusterImageCatalog lives outside the namespace this
// console is otherwise confined to, so every way the lookup can decline
// to produce content is named rather than collapsed into an absence.
type ClusterCatalogState string

const (
	// ClusterCatalogDisabled means the deployment did not opt in. The
	// reference is shown and its content is not claimed.
	ClusterCatalogDisabled ClusterCatalogState = "disabled"
	// ClusterCatalogNotReferenced means the cluster draws its image from
	// a namespaced catalog, or names it directly.
	ClusterCatalogNotReferenced ClusterCatalogState = "not-referenced"
	// ClusterCatalogPresent means the referenced catalog was read.
	ClusterCatalogPresent ClusterCatalogState = "present"
	// ClusterCatalogAbsent means the API server confirmed the referenced
	// catalog does not exist.
	ClusterCatalogAbsent ClusterCatalogState = "absent"
	// ClusterCatalogUnknown means the lookup could not be made. A denied
	// ClusterRole binding and a missing CRD both land here: the console
	// says it could not look, never that the catalog is not there.
	ClusterCatalogUnknown ClusterCatalogState = "unknown"
)

// CatalogImageFacts is one image a catalog offers.
type CatalogImageFacts struct {
	// Major is the PostgreSQL major version the image provides.
	Major int
	// Image is the image reference.
	Image string
}

// ImageCatalogFacts is the operator-reported content of one
// ImageCatalog.
//
// A catalog is namespace-shared infrastructure: it is not owned by a
// cluster, clusters reference it. So unlike every other collection this
// package retains, the set is not filtered to the target cluster at the
// source — the console renders only the catalog the cluster's
// spec.imageCatalogRef names, and that resolution happens at render,
// where the cluster snapshot is available and a changed reference is
// picked up without restarting a watch.
type ImageCatalogFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Images is the bounded list the catalog offers, major-ascending.
	Images []CatalogImageFacts
	// ImagesTruncated reports that the catalog carried more images than
	// the display bound.
	ImagesTruncated bool
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
}

// ImageCatalogsSnapshot is the rendered catalog set, immutable and
// carrying its own staleness.
type ImageCatalogsSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact.
	Stale bool
	// Truncated reports that more catalogs existed than the bound.
	Truncated bool
	// Catalogs is sorted by name; the producer bounds it. These
	// are the namespace's catalogs.
	Catalogs []ImageCatalogFacts
	// ClusterCatalog is the cluster-scoped catalog the Cluster
	// references, read only when the deployment opted in. It is kept
	// apart from Catalogs because a namespaced and a cluster-scoped
	// catalog may share a name and mean different objects.
	ClusterCatalog ImageCatalogFacts
	// ClusterCatalogState is the outcome of that optional lookup.
	ClusterCatalogState ClusterCatalogState
}

// Catalog returns the named catalog and whether it was observed.
func (s ImageCatalogsSnapshot) Catalog(name string) (ImageCatalogFacts, bool) {
	for _, c := range s.Catalogs {
		if c.Name == name {
			return c, true
		}
	}
	return ImageCatalogFacts{}, false
}
