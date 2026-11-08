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

package selfresolve

import (
	"fmt"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// NewInCluster builds the Resolver (options without Reader) and its
// Watcher on the Pod's own ServiceAccount: a namespaced, uncached client (Secrets are read on
// demand, never cached) with a fixed REST mapping of the kinds it
// reads, so the agent needs no discovery and no other permission than the
// chart's Role.
func NewInCluster(options Options) (*Resolver, *Watcher, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("self-resolution needs the in-cluster API: %w", err)
	}
	restConfig.UserAgent = "pgtoolbox-evidence-agent"
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(cnpgv1.SchemeGroupVersion.WithKind("Cluster"), meta.RESTScopeNamespace)
	mapper.Add(discovery.ObjectStoreGVK, meta.RESTScopeNamespace)
	mapper.Add(discovery.StanzaGVK, meta.RESTScopeNamespace)
	mapper.Add(discovery.ArchiveGVK, meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	direct, err := client.New(restConfig, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, nil, fmt.Errorf("self-resolution client: %w", err)
	}
	options.Reader = client.NewNamespacedClient(direct, options.Namespace)
	resolver, err := New(options)
	if err != nil {
		return nil, nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("self-resolution watch client: %w", err)
	}
	watcher, err := NewWatcher(dynamicClient, resolver)
	if err != nil {
		return nil, nil, err
	}
	return resolver, watcher, nil
}
