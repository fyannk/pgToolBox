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

package kube

import (
	"sort"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// convertImageCatalog converts a raw catalog into facts. The image list
// is ordered by major version and bounded here, at the boundary.
func convertImageCatalog(content map[string]any) (observe.ImageCatalogFacts, error) {
	var catalog cnpgv1.ImageCatalog
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &catalog); err != nil {
		return observe.ImageCatalogFacts{}, redact.NewError("image catalog convert", redact.CategoryInternal, err)
	}
	facts := observe.ImageCatalogFacts{
		Name:      catalog.Name,
		UID:       string(catalog.UID),
		CreatedAt: catalog.CreationTimestamp.Time.UTC(),
	}
	images := catalog.Spec.Images
	if len(images) > observe.MaxCatalogImages {
		images = images[:observe.MaxCatalogImages]
		facts.ImagesTruncated = true
	}
	for _, img := range images {
		facts.Images = append(facts.Images, observe.CatalogImageFacts{Major: img.Major, Image: img.Image})
	}
	sort.Slice(facts.Images, func(i, j int) bool { return facts.Images[i].Major < facts.Images[j].Major })
	return facts, nil
}
