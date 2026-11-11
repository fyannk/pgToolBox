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
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

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
	optionalKindCacheOptions(options, prepared.Config.WatchNamespaces, prepared.OptionalKinds)
	secretCacheOptions(options)
}

// optionalKindCacheOptions restricts the informer of each optional kind
// refused in some watched namespaces to the namespaces where it can be
// listed (ByObject.Namespaces; the kind's label filter and transform are
// kept): an informer listing in a namespace that refuses it would never
// sync, and the cache would never be ready. The controllers ask the
// feature per namespace (Features.In) and read the kind only where it is
// on, so the cache is never asked for a namespace it does not hold.
func optionalKindCacheOptions(options *ctrl.Options, watched []string, kinds []optionalKind) {
	for _, kind := range kinds {
		listable := kind.Result.ListableIn(watched)
		if len(listable) == 0 {
			continue
		}
		namespaces := make(map[string]cache.Config, len(listable))
		for _, namespace := range listable {
			namespaces[namespace] = cache.Config{}
		}
		if options.Cache.ByObject == nil {
			options.Cache.ByObject = map[client.Object]cache.ByObject{}
		}
		objectScheme := options.Scheme
		if objectScheme == nil {
			objectScheme = scheme
		}
		key := kind.Object
		for object := range options.Cache.ByObject {
			if gvk, err := apiutil.GVKForObject(object, objectScheme); err == nil && gvk == kind.Result.GVK {
				key = object
				break
			}
		}
		byObject := options.Cache.ByObject[key]
		byObject.Namespaces = namespaces
		options.Cache.ByObject[key] = byObject
	}
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
