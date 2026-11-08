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

package backupresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// The barman-cloud plugin's registration and its Cluster parameters
// (plugin-barman-cloud internal/cnpgi/operator/config: barmanObjectName
// names the ObjectStore, serverName overrides the Barman server name, which
// otherwise is the Cluster's name — the ObjectStore's own
// configuration.serverName is not consulted by the plugin, so it is not
// consulted here either).
const (
	BarmanPluginName      = "barman-cloud.cloudnative-pg.io"
	BarmanObjectParameter = "barmanObjectName"
	BarmanServerParameter = "serverName"
)

// SecretKeyRef names one key of one same-namespace Secret, as the
// ObjectStore names it. The resolver never reads the value.
type SecretKeyRef struct {
	Name string
	Key  string
}

// Problem is an expected outcome of resolution, not an error: a missing
// object or a repository shape the agent does not read. The operator
// publishes it as ConfigurationValid=False with Reason; the self-resolving
// agent serves unknown evidence with it as the repository error. The
// message names objects, never a value read from them.
type Problem struct {
	Reason  pgtoolboxv1alpha1.Reason
	Message string
}

// Error makes a Problem usable as the agent's repository error.
func (p *Problem) Error() string { return string(p.Reason) + ": " + p.Message }

func problem(reason pgtoolboxv1alpha1.Reason, format string, args ...any) *Problem {
	return &Problem{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Request names what to resolve. ObjectStoreName and ServerName override
// what the Cluster says (EvidenceAgent spec.objectStoreRef and
// spec.serverName); empty follows the Cluster. PgBackRest is the author's
// pgBackRest settings, which only a Cluster archiving with a pgBackRest
// plugin accepts.
type Request struct {
	Namespace       string
	ClusterName     string
	ObjectStoreName string
	ServerName      string
	PgBackRest      *PgBackRestSettings
}

// Located is the first step of resolution: the Cluster and where its backup
// configuration lives. Exactly one of ObjectStoreName (the barman-cloud
// plugin's ObjectStore, read next), InTree (the Cluster's own
// spec.backup.barmanObjectStore, already read) and PgBackRest (a pgBackRest
// plugin's Stanza or Archive, read next) is set.
type Located struct {
	Cluster         *cnpgv1.Cluster
	ObjectStoreName string
	InTree          *barmanapi.BarmanObjectStoreConfiguration
	// ServerName is the Barman server; empty for pgBackRest.
	ServerName string
	PgBackRest *PgBackRestLocation
}

// Origin names where the configuration was read, for messages.
func (l *Located) Origin() Origin {
	origin := Origin{Cluster: l.Cluster.Name, ObjectStore: l.ObjectStoreName}
	if l.PgBackRest != nil {
		origin.PgBackRestKind, origin.PgBackRestObject = l.PgBackRest.Kind, l.PgBackRest.ObjectName
	}
	return origin
}

// Origin is where a backup configuration was read: the plugin's
// ObjectStore, a pgBackRest plugin's Stanza or Archive or, when both are
// empty, the Cluster's in-tree spec.backup.barmanObjectStore.
type Origin struct {
	Cluster     string
	ObjectStore string
	// PgBackRestKind (Stanza or Archive) and PgBackRestObject name a
	// pgBackRest plugin's object.
	PgBackRestKind   string `json:",omitempty"`
	PgBackRestObject string `json:",omitempty"`
}

// String names the origin in a message.
func (o Origin) String() string {
	switch {
	case o.PgBackRestKind != "":
		return o.PgBackRestKind + " " + o.PgBackRestObject
	case o.ObjectStore != "":
		return "ObjectStore " + o.ObjectStore
	default:
		return "Cluster " + o.Cluster + " spec.backup.barmanObjectStore"
	}
}

// Locate reads the Cluster and selects its backup configuration (and the
// Barman server name), in this order: an explicit ObjectStore (request),
// the enabled barman-cloud plugin stanza's barmanObjectName, the in-tree
// spec.backup.barmanObjectStore, an enabled pgBackRest plugin (Dalibo's
// stanzaRef, then Opera's pgbackrestObjectName). The plugin wins over the
// in-tree stanza because CloudNativePG archives through the plugin when
// both are present.
// A Located is returned with a Problem when the Cluster exists but has no
// Barman configuration, so a caller can still record what it read.
func Locate(ctx context.Context, reader client.Reader, request Request) (*Located, *Problem, error) {
	var cluster cnpgv1.Cluster
	if err := reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.ClusterName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, problem(pgtoolboxv1alpha1.ReasonClusterNotFound, "Cluster %s was not found in namespace %s", request.ClusterName, request.Namespace), nil
		}
		return nil, nil, fmt.Errorf("read Cluster %s: %w", request.ClusterName, err)
	}
	plugin := BarmanPluginParameters(&cluster)
	located := &Located{Cluster: &cluster, ObjectStoreName: plugin[BarmanObjectParameter], ServerName: cluster.Name}
	if request.ObjectStoreName != "" {
		located.ObjectStoreName = request.ObjectStoreName
	}
	switch {
	case located.ObjectStoreName != "":
		if name := plugin[BarmanServerParameter]; name != "" {
			located.ServerName = name
		}
	case cluster.Spec.Backup != nil && cluster.Spec.Backup.BarmanObjectStore != nil:
		located.InTree = cluster.Spec.Backup.BarmanObjectStore.DeepCopy()
		if name := located.InTree.ServerName; name != "" {
			located.ServerName = name
		}
	}
	if request.ServerName != "" {
		located.ServerName = request.ServerName
	}
	if located.ObjectStoreName == "" && located.InTree == nil {
		if entry := enabledPgBackRestPlugin(pgBackRestPlugins(&cluster)); entry != nil {
			located, whyNot := locatePgBackRest(located, request, entry)
			return located, whyNot, nil
		}
		return located, problem(pgtoolboxv1alpha1.ReasonBarmanPluginNotConfigured,
			"Cluster %s declares neither an enabled barman-cloud plugin with %s, spec.backup.barmanObjectStore nor an enabled pgBackRest plugin (%s), and no ObjectStore is named explicitly",
			cluster.Name, BarmanObjectParameter, strings.Join(PgBackRestPluginNames, ", ")), nil
	}
	if request.PgBackRest != nil {
		return located, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"the agent's pgbackrest settings (EvidenceAgent spec.pgbackrest, pgtools evidence.agents[].pgbackrest) apply to a Cluster "+
				"archiving with a pgBackRest plugin, but Cluster %s is read from %s: remove them", cluster.Name, located.Origin()), nil
	}
	return located, nil, nil
}

// pgBackRestPlugins lists the Cluster's plugin entries.
func pgBackRestPlugins(cluster *cnpgv1.Cluster) []pgBackRestPluginEntry {
	entries := make([]pgBackRestPluginEntry, 0, len(cluster.Spec.Plugins))
	for i := range cluster.Spec.Plugins {
		plugin := &cluster.Spec.Plugins[i]
		entries = append(entries, pgBackRestPluginEntry{
			name: plugin.Name, enabled: plugin.Enabled == nil || *plugin.Enabled, parameters: plugin.Parameters,
		})
	}
	return entries
}

// Configuration returns the located Barman configuration: the in-tree one,
// or the ObjectStore's, read through reader (ReadObjectStore). A pgBackRest
// location has none: ReadPgBackRest and ComposePgBackRest read it.
func Configuration(ctx context.Context, reader client.Reader, located *Located) (*barmanapi.BarmanObjectStoreConfiguration, *Problem, error) {
	if located.PgBackRest != nil {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s is a pgBackRest configuration, not a Barman one", located.Origin()), nil
	}
	if located.InTree != nil {
		return located.InTree, nil, nil
	}
	return ReadObjectStore(ctx, reader, located.Cluster.Namespace, located.ObjectStoreName)
}

// BarmanPluginParameters returns the parameters of the Cluster's enabled
// barman-cloud plugin stanza, or nil.
func BarmanPluginParameters(cluster *cnpgv1.Cluster) map[string]string {
	for i := range cluster.Spec.Plugins {
		plugin := &cluster.Spec.Plugins[i]
		if plugin.Name != BarmanPluginName {
			continue
		}
		if plugin.Enabled != nil && !*plugin.Enabled {
			return nil
		}
		return plugin.Parameters
	}
	return nil
}

var errUndecodableObjectStore = errors.New("spec.configuration is missing or not a Barman object store configuration")

// ReadObjectStore reads the ObjectStore unstructured (neither caller
// registers a scheme for an optional CRD) and decodes only
// spec.configuration into the Barman type vendored through CloudNativePG.
// NotFound, an unserved kind and an undecodable configuration are
// Problems; anything else is an error.
func ReadObjectStore(ctx context.Context, reader client.Reader, namespace, name string) (*barmanapi.BarmanObjectStoreConfiguration, *Problem, error) {
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(discovery.ObjectStoreGVK)
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, store)
	switch {
	case apierrors.IsNotFound(err):
		return nil, problem(pgtoolboxv1alpha1.ReasonObjectStoreNotFound, "ObjectStore %s was not found in namespace %s", name, namespace), nil
	case meta.IsNoMatchError(err):
		return nil, problem(pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable, "the barmancloud.cnpg.io/v1 ObjectStore API is not served"), nil
	case err != nil:
		return nil, nil, fmt.Errorf("read ObjectStore %s: %w", name, err)
	}
	configuration, err := DecodeObjectStore(store)
	if err != nil {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "ObjectStore %s: %v", name, err), nil
	}
	return configuration, nil, nil
}

// DecodeObjectStore decodes spec.configuration of an unstructured
// ObjectStore.
func DecodeObjectStore(store *unstructured.Unstructured) (*barmanapi.BarmanObjectStoreConfiguration, error) {
	raw, found, err := unstructured.NestedMap(store.Object, "spec", "configuration")
	if err != nil || !found {
		return nil, errUndecodableObjectStore
	}
	configuration := &barmanapi.BarmanObjectStoreConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, configuration); err != nil {
		return nil, errUndecodableObjectStore
	}
	return configuration, nil
}
