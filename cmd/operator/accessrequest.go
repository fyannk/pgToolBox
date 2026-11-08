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

package main

// PgAccessRequest wiring of the operator binary. Kept in its own file
// like the EvidenceAgent's; main.go calls the two functions below.

import (
	"context"
	"reflect"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/internal/operator/accessrequest"
)

// accessRequestCacheOptions adds the RoleBinding filter when no other
// controller set one (they all select managed-by=pgtoolbox).
func accessRequestCacheOptions(options *ctrl.Options) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	present := map[reflect.Type]bool{}
	for object := range options.Cache.ByObject {
		present[reflect.TypeOf(object)] = true
	}
	for object, byObject := range accessrequest.CacheByObject() {
		if !present[reflect.TypeOf(object)] {
			options.Cache.ByObject[object] = byObject
		}
	}
}

// setupAccessRequest registers the controller and returns the kinds its
// informers hold (pgtoolbox_cached_objects).
func setupAccessRequest(ctx context.Context, mgr ctrl.Manager) ([]client.Object, error) {
	if err := metrics.Registry.Register(accessrequest.Collector{Reader: mgr.GetCache()}); err != nil {
		return nil, err
	}
	return accessrequest.CachedKinds(), (&accessrequest.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("pgaccessrequest"),
	}).SetupWithManager(ctx, mgr)
}
