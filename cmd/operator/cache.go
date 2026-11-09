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

import (
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/console"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
)

// managerCacheOptions shapes the manager's cache for every controller it
// runs: each controller's label-filtered informers, then the Secret
// transform on top of them all.
func managerCacheOptions(options *ctrl.Options, prepared pgAdminPrepared, consoleFeatures console.Features) {
	evidenceAgentCacheOptions(options)
	pgAdminManagerOptions(options, prepared)
	accessRequestCacheOptions(options)
	pgToolBoxConsoleCacheOptions(options, consoleFeatures)
	secretCacheOptions(options)
}

// secretCacheOptions gives the Secret informers (metadata-only: Secrets are
// never cached for content) a transform that keeps no annotation and only
// the labels the operator reads, so a kubectl last-applied-configuration
// annotation, which holds the Secret's data, never enters the cache. The
// Secret informer is shared by the controllers and not label-filtered (user
// Secrets are watched by name); a filter set by a controller is kept.
func secretCacheOptions(options *ctrl.Options) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	for object, byObject := range options.Cache.ByObject {
		if _, isSecret := object.(*corev1.Secret); isSecret {
			byObject.Transform = metadata.TransformSecretMetadata()
			options.Cache.ByObject[object] = byObject
			return
		}
	}
	options.Cache.ByObject[&corev1.Secret{}] = cache.ByObject{Transform: metadata.TransformSecretMetadata()}
}
