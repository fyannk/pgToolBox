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

package cache

import (
	"context"
	"fmt"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Field indexes: every reverse lookup the read model makes is an index,
// never a scan of the namespace.
const (
	// IndexCluster is the owning Cluster's name, on Backups,
	// ScheduledBackups, Poolers and the declarative objects, and the
	// ClusterLabel value on Pods and PVCs; use it with
	// client.MatchingFields{IndexCluster: name}. (A label selector is not
	// indexed: the cache would match every object of the namespace.)
	IndexCluster = "pgtoolbox.cluster"
	// IndexInvolved is "<Kind>/<name>" of an Event's involved object.
	IndexInvolved = "pgtoolbox.involved"
)

// InvolvedKey is the IndexInvolved value of an object.
func InvolvedKey(kind, name string) string { return kind + "/" + name }

// Index is one field index the cache registers on a watched kind.
type Index struct {
	Source string
	Object client.Object
	Field  string
	Func   client.IndexerFunc
}

// Indexes lists every field index of the console's cache; tests register
// the same functions on a fake client.
func Indexes() []Index {
	return []Index{
		{SourceBackups, &cnpgv1.Backup{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.Backup).Spec.Cluster.Name) }},
		{SourceScheduledBackups, &cnpgv1.ScheduledBackup{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.ScheduledBackup).Spec.Cluster.Name) }},
		{SourcePoolers, &cnpgv1.Pooler{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.Pooler).Spec.Cluster.Name) }},
		{SourceDatabases, &cnpgv1.Database{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.Database).Spec.ClusterRef.Name) }},
		{SourceDatabaseRoles, &cnpgv1.DatabaseRole{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.DatabaseRole).Spec.ClusterRef.Name) }},
		{SourcePublications, &cnpgv1.Publication{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.Publication).Spec.ClusterRef.Name) }},
		{SourceSubscriptions, &cnpgv1.Subscription{}, IndexCluster, func(o client.Object) []string { return nonEmpty(o.(*cnpgv1.Subscription).Spec.ClusterRef.Name) }},
		{SourcePods, &corev1.Pod{}, IndexCluster, clusterLabel},
		{SourcePVCs, &corev1.PersistentVolumeClaim{}, IndexCluster, clusterLabel},
		{SourceEvents, &corev1.Event{}, IndexInvolved, func(o client.Object) []string {
			ref := o.(*corev1.Event).InvolvedObject
			return []string{InvolvedKey(ref.Kind, ref.Name)}
		}},
	}
}

// registerIndexes adds the indexes of the watched sources only: indexing
// a kind starts its informer, and a kind the plan left out (not
// installed, forbidden) must not be watched.
func registerIndexes(ctx context.Context, c crcache.Cache, watched map[string]bool) error {
	for _, index := range Indexes() {
		if !watched[index.Source] {
			continue
		}
		if err := c.IndexField(ctx, index.Object, index.Field, index.Func); err != nil {
			return fmt.Errorf("cache: index %s on %s: %w", index.Field, index.Source, err)
		}
	}
	return nil
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func clusterLabel(o client.Object) []string { return nonEmpty(o.GetLabels()[ClusterLabel]) }
