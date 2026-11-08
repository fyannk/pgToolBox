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
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// Facts converts objects the console read from its informer cache into
// the observe facts pkg/diagnose reads, for one Cluster, through the
// pkg/cnpgstatus/observe converters: membership (controller owner
// reference), bounding, event candidacy and the plugin-backup rule are
// therefore those of the read model. It performs no I/O; the zero-value
// Client it wraps has no API client and is never asked for one.
type Facts struct {
	c *Client
}

// NewFacts returns the converters for one Cluster.
func NewFacts(namespace, cluster string) Facts {
	return Facts{c: &Client{
		opts:   target{Namespace: namespace, ClusterName: cluster},
		logger: slog.New(slog.DiscardHandler),
	}}
}

// content turns a typed object into the unstructured form the
// converters take.
func content(obj runtime.Object) (map[string]any, error) {
	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, redact.NewError("object convert", redact.CategoryInternal, err)
	}
	return out, nil
}

// Cluster converts the Cluster.
func (f Facts) Cluster(obj runtime.Object) (observe.ClusterFacts, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.ClusterFacts{}, err
	}
	return convertCluster(raw)
}

// Pod converts a pod and reports whether it is a proven member of the
// Cluster (controlled by it).
func (f Facts) Pod(obj runtime.Object) (observe.PodFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.PodFacts{}, false, err
	}
	return f.c.convertPod(raw)
}

// Event converts an event and reports whether it concerns the Cluster.
func (f Facts) Event(obj runtime.Object) (observe.EventFacts, bool) {
	raw, err := content(obj)
	if err != nil {
		return observe.EventFacts{}, false
	}
	return f.c.convertEvent(raw)
}

// Backup converts a Backup and reports whether it belongs to the Cluster.
func (f Facts) Backup(obj runtime.Object) (observe.BackupFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.BackupFacts{}, false, err
	}
	return f.c.convertBackup(raw)
}

// ScheduledBackup converts a ScheduledBackup and reports whether it
// belongs to the Cluster.
func (f Facts) ScheduledBackup(obj runtime.Object) (observe.ScheduledBackupFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.ScheduledBackupFacts{}, false, err
	}
	return f.c.convertScheduledBackup(raw)
}

// Pooler converts a Pooler and reports whether it belongs to the Cluster.
func (f Facts) Pooler(obj runtime.Object) (observe.PoolerFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.PoolerFacts{}, false, err
	}
	return f.c.convertPooler(raw)
}

// FailoverQuorum converts the Cluster's FailoverQuorum.
func (f Facts) FailoverQuorum(obj runtime.Object) (observe.FailoverQuorumFacts, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.FailoverQuorumFacts{}, err
	}
	return convertFailoverQuorum(raw)
}

// PrimaryLease converts the Cluster's primary Lease.
func (f Facts) PrimaryLease(obj runtime.Object) (observe.PrimaryLeaseFacts, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.PrimaryLeaseFacts{}, err
	}
	return convertPrimaryLease(raw)
}

// Database converts a Database and reports whether it targets the
// Cluster.
func (f Facts) Database(obj runtime.Object) (observe.DatabaseFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.DatabaseFacts{}, false, err
	}
	return f.c.convertDatabase(raw)
}

// DatabaseRole converts a DatabaseRole and reports whether it targets
// the Cluster.
func (f Facts) DatabaseRole(obj runtime.Object) (observe.DatabaseRoleFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.DatabaseRoleFacts{}, false, err
	}
	return f.c.convertDatabaseRole(raw)
}

// Publication converts a Publication and reports whether it targets the
// Cluster.
func (f Facts) Publication(obj runtime.Object) (observe.PublicationFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.PublicationFacts{}, false, err
	}
	return f.c.convertPublication(raw)
}

// Subscription converts a Subscription and reports whether it targets
// the Cluster.
func (f Facts) Subscription(obj runtime.Object) (observe.SubscriptionFacts, bool, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.SubscriptionFacts{}, false, err
	}
	return f.c.convertSubscription(raw)
}

// ImageCatalog converts an ImageCatalog or a ClusterImageCatalog (the
// same spec).
func (f Facts) ImageCatalog(obj runtime.Object) (observe.ImageCatalogFacts, error) {
	raw, err := content(obj)
	if err != nil {
		return observe.ImageCatalogFacts{}, err
	}
	return convertImageCatalog(raw)
}

// ObjectStoreReference reads the Cluster's barman-cloud ObjectStore
// reference from the informer cache's objects exactly as the observe
// fetch does: not-referenced without an enabled plugin stanza, unknown when the
// store could not be read (readable false) or is absent from the cache,
// present with its destination, endpoint, retention and this server's
// recovery window otherwise.
func (f Facts) ObjectStoreReference(cluster runtime.Object, store *unstructured.Unstructured, readable bool) observe.ObjectStoreReference {
	unknown := observe.ObjectStoreReference{State: observe.ObjectStoreUnknown}
	raw, err := content(cluster)
	if err != nil {
		return unknown
	}
	name, serverName, err := objectStoreName(raw)
	switch {
	case err != nil:
		return unknown
	case name == "":
		return observe.ObjectStoreReference{State: observe.ObjectStoreNotReferenced}
	}
	if serverName == "" {
		serverName = f.c.opts.ClusterName
	}
	ref := observe.ObjectStoreReference{Name: name, ServerName: serverName, State: observe.ObjectStoreUnknown}
	if !readable || store == nil {
		return ref
	}
	ref.State = observe.ObjectStorePresent
	ref.Destination, _, _ = unstructured.NestedString(store.Object, "spec", "configuration", "destinationPath")
	ref.Endpoint, _, _ = unstructured.NestedString(store.Object, "spec", "configuration", "endpointURL")
	ref.RetentionPolicy, _, _ = unstructured.NestedString(store.Object, "spec", "retentionPolicy")
	if window, found, _ := unstructured.NestedMap(store.Object, "status", "serverRecoveryWindow", serverName); found {
		ref.RecoveryWindow = &observe.RecoveryWindow{
			FirstRecoverabilityPoint: nestedInstant(window, "firstRecoverabilityPoint"),
			LastSuccessfulBackup:     nestedInstant(window, "lastSuccessfulBackupTime"),
			LastFailedBackup:         nestedInstant(window, "lastFailedBackupTime"),
		}
	}
	return ref
}

// Observation captures a typed object for the history store exactly as
// the history capture does (normalized, scrubbed of managed fields, resource
// version, last-applied and inline env values, spec and status digested
// apart). The informer cache strips managedFields before an object reaches
// here, so revisions it records are unattributed: honest, not inferred.
// Objects read from a typed cache carry no TypeMeta, so the kind comes
// from the caller, which knows what it watched.
func Observation(scope string, gvk schema.GroupVersionKind, obj runtime.Object, deleted bool) (history.Observation, error) {
	raw, err := content(obj)
	if err != nil {
		return history.Observation{}, err
	}
	raw["apiVersion"], raw["kind"] = gvk.GroupVersion().String(), gvk.Kind
	return observationFrom(scope, raw, deleted)
}
