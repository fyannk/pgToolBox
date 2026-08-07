/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package shared

import (
	"context"
	"encoding/json"
	"fmt"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Runtime is what every application reconciler in the family needs from the
// manager, independent of which application it reconciles. Each application's
// own reconciler embeds it and adds only what is genuinely its own.
type Runtime struct {
	client.Client

	// APIReader performs the live (non-cached) reads used for Secret and
	// ConfigMap content: those kinds are watched metadata-only and their
	// data is never held in the informer cache.
	APIReader client.Reader

	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Optional platform watches and typed objects are enabled only when API
	// discovery found their CRDs at manager startup. They gate exposure
	// behaviour identically for every application.
	RouteAPIAvailable   bool
	GatewayAPIAvailable bool
}

// ApplyObject submits desired as a server-side apply patch under the
// operator's field owner, so fields written by other actors or API-server
// defaulting are preserved instead of being clobbered by a full update.
func (r Runtime) ApplyObject(ctx context.Context, desired client.Object) error {
	applyData, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("marshal %T apply patch: %w", desired, err)
	}
	applyPatch := client.RawPatch(types.ApplyPatchType, applyData)
	if err := r.Patch(
		ctx,
		desired,
		applyPatch,
		client.FieldOwner(pgtoolboxv1alpha1.ManagedByLabelValue),
	); err != nil {
		return fmt.Errorf("apply %T %s/%s: %w", desired, desired.GetNamespace(), desired.GetName(), err)
	}
	return nil
}
