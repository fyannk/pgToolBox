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

package ops

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// Restore drills. Two writes, both pinned here:
//
//   - the drill Cluster, created by UserWriter.CreateDrillCluster as the
//     signed-in person (token passthrough: the API server decides whether
//     they may create a Cluster in the drill namespace);
//   - its deletion, by DrillReaper with the console's own ServiceAccount
//     through the opt-in teardown Role (get/list/watch/delete on
//     clusters.postgresql.cnpg.io in the drill namespace, nothing else),
//     because the person's session may be gone when the drill ends.
//
// The reaper deletes only a Cluster this console sealed: the seal is an
// HMAC, under a key derived from the console's session secret, over the
// drill's namespace, name and creation record. Labels alone prove
// nothing (anyone who may create a Cluster can set them); the seal cannot
// be produced without the console's secret, and it is bound to one name,
// so it cannot be moved onto another Cluster.

// Drill labels and annotations. The keys are API surface: extend, never
// rename.
const (
	// LabelRestoreDrillOf names the source Cluster of a drill.
	LabelRestoreDrillOf = "pgtoolbox.fyannk.dev/restore-drill-of"
	// LabelRestoreDrillSourceNamespace names the source's namespace.
	LabelRestoreDrillSourceNamespace = "pgtoolbox.fyannk.dev/restore-drill-source-namespace"
	// LabelDrillOwner names the console that created the drill (its
	// namespace: one console per team namespace).
	LabelDrillOwner = "pgtoolbox.fyannk.dev/drill-owner"
	// AnnotationRestoreDrill is the drill's creation record (DrillMeta).
	AnnotationRestoreDrill = "pgtoolbox.fyannk.dev/restore-drill"
	// AnnotationRestoreDrillSeal is the HMAC over the record.
	AnnotationRestoreDrillSeal = "pgtoolbox.fyannk.dev/restore-drill-seal"
	// DrillKeyLabel is the HKDF label of the seal's keys
	// (auth.PurposeKeys).
	DrillKeyLabel = "pgtoolbox-console/v1/restore-drill-seal"
	// DrillSourceName is the externalClusters entry the drill recovers
	// from.
	DrillSourceName = "drill-source"
	// MaxClusterName is CloudNativePG's limit on a Cluster name (its
	// webhook refuses longer ones: the instance and service names derive
	// from it).
	MaxClusterName = 50
	// BarmanCloudPlugin is the barman-cloud plugin's name.
	BarmanCloudPlugin = "barman-cloud.cloudnative-pg.io"
	// PgBackRestDaliboPlugin and PgBackRestOperaPlugin are the two
	// pgBackRest plugins.
	// (cnpgstatus is the source of truth.)
	PgBackRestDaliboPlugin = cnpgstatus.PgBackRestDaliboPlugin
	PgBackRestOperaPlugin  = cnpgstatus.PgBackRestOperaPlugin
	// The plugins' Cluster parameters a drill carries over: Dalibo's
	// Stanza and optional PluginConfig, Opera's Archive and stanza.
	daliboStanzaRef       = cnpgstatus.PgBackRestDaliboObject
	daliboPluginConfigRef = "pluginConfigRef"
	operaObjectName       = cnpgstatus.PgBackRestOperaObject
	operaStanza           = cnpgstatus.PgBackRestOperaStanza
)

// DrillParameters are the PostgreSQL settings every drill runs with,
// whatever the source sets: a promoted drill is a live copy of the
// database, and these are the background workers that would act on the
// world outside it. max_logical_replication_workers=0 keeps the logical
// replication launcher from starting, so no subscription reconnects to its
// publisher (with the credentials in pg_subscription) or takes over its
// slot; recovery does not require it to match the primary's. pg_cron
// (1.5 or later) runs no job with cron.launch_active_jobs off, so nothing
// scheduled calls dblink, postgres_fdw, pg_net or COPY TO PROGRAM; and
// TimescaleDB runs no job without background workers. A setting whose
// library is not loaded is an inert placeholder, so all three are set
// always: a library can be preloaded through more than one field.
// Neither CloudNativePG nor PostgreSQL fixes any of them.
var DrillParameters = map[string]string{
	"max_logical_replication_workers":    "0",
	"cron.launch_active_jobs":            "off",
	"timescaledb.max_background_workers": "0",
}

// DrillCreationSkew bounds how far a drill's server-set creation time may
// lie from the StartedAt of its sealed record: before by clock skew,
// after by the request's latency. A record copied onto a Cluster created
// later does not verify.
const (
	DrillCreationSkewBefore = 2 * time.Minute
	DrillCreationSkewAfter  = 5 * time.Minute
)

// DrillMeta is the creation record a drill carries, sealed.
type DrillMeta struct {
	V               int       `json:"v"`
	SourceNamespace string    `json:"sourceNamespace"`
	Source          string    `json:"source"`
	SourceUID       string    `json:"sourceUID,omitempty"`
	Backup          string    `json:"backup,omitempty"`
	BackupStoppedAt time.Time `json:"backupStoppedAt,omitzero"`
	StartedAt       time.Time `json:"startedAt"`
	Deadline        time.Time `json:"deadline"`
	// SourceBytes is the source primary's database size (exporter) when
	// the drill started; absent when the exporter did not report it.
	SourceBytes *int64 `json:"sourceBytes,omitempty"`
	Owner       string `json:"owner"`
	// BackupLabel is the pgBackRest backup the drill restores
	// (recoveryTarget.backupID), from the evidence agent's recovery
	// window; empty for Barman.
	BackupLabel string `json:"backupLabel,omitempty"`
}

// RecoverySource is where a drill restores from: the barman-cloud plugin
// (an ObjectStore in the drill's own namespace), a pgBackRest plugin (its
// Stanza or Archive, which both resolve in the recovering Cluster's
// namespace) or the source's in-tree barmanObjectStore.
type RecoverySource struct {
	Plugin bool
	// PgBackRest is set for a pgBackRest plugin (Plugin is then false).
	PgBackRest *PgBackRestSource
	// ObjectStore is the plugin's ObjectStore name.
	ObjectStore string
	// ServerName is the folder of the source's backups in the repository.
	ServerName string
	// InTree is the source's in-tree configuration (credentials are
	// Secrets of the source's namespace).
	InTree *cnpgv1.BarmanObjectStoreConfiguration
}

// PgBackRestSource is a pgBackRest recovery source: the plugin and the
// parameters the source archives with, restricted to the ones that name
// where to read.
type PgBackRestSource struct {
	Plugin string
	// Object is the Stanza (Dalibo stanzaRef) or Archive (Opera
	// pgbackrestObjectName) the source names.
	Object string
	// Stanza is Opera's backup stanza: its parameter, else the source
	// Cluster's name (Opera's default), always written on the drill since
	// the default would otherwise be the drill's own name. Empty for
	// Dalibo, whose Stanza object names it.
	Stanza string
	// PluginConfig is Dalibo's optional pluginConfigRef.
	PluginConfig string
	// Target is the backup label the drill restores (recoveryTarget.
	// backupID), from the evidence agent's recovery window; the caller
	// sets it.
	Target string
}

// Parameters are the external cluster's plugin parameters: the same
// keys and values the source uses to name its repository, nothing else.
func (p PgBackRestSource) Parameters() map[string]string {
	if p.Plugin == PgBackRestOperaPlugin {
		return map[string]string{operaObjectName: p.Object, operaStanza: p.Stanza}
	}
	out := map[string]string{daliboStanzaRef: p.Object}
	if p.PluginConfig != "" {
		out[daliboPluginConfigRef] = p.PluginConfig
	}
	return out
}

// ErrNoRecoverySource is returned for a source that backs up neither
// with a supported backup plugin nor with an in-tree barmanObjectStore
// (volume snapshots are not drilled).
var ErrNoRecoverySource = errors.New("the Cluster backs up neither with the barman-cloud or a pgBackRest plugin nor with barmanObjectStore")

// DrillRecoverySource reads the source's archive: the enabled
// barman-cloud plugin (by name: another plugin's barmanObjectName means
// whatever that plugin decides) with a barmanObjectName, an enabled
// pgBackRest plugin naming its object, else spec.backup.barmanObjectStore.
func DrillRecoverySource(source *cnpgv1.Cluster) (RecoverySource, error) {
	for _, p := range source.Spec.Plugins {
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		if pb, ok := pgbackrestSource(source.Name, p); ok {
			return RecoverySource{PgBackRest: &pb}, nil
		}
		if p.Name != BarmanCloudPlugin {
			continue
		}
		store := p.Parameters["barmanObjectName"]
		if store == "" {
			continue
		}
		server := p.Parameters["serverName"]
		if server == "" {
			server = source.Name
		}
		return RecoverySource{Plugin: true, ObjectStore: store, ServerName: server}, nil
	}
	if source.Spec.Backup != nil && source.Spec.Backup.BarmanObjectStore != nil {
		bos := source.Spec.Backup.BarmanObjectStore.DeepCopy()
		server := bos.ServerName
		if server == "" {
			server = source.Name
		}
		bos.ServerName = server
		return RecoverySource{InTree: bos, ServerName: server}, nil
	}
	return RecoverySource{}, ErrNoRecoverySource
}

// pgbackrestSource reads a pgBackRest plugin entry of the source.
func pgbackrestSource(cluster string, p cnpgv1.PluginConfiguration) (PgBackRestSource, bool) {
	switch p.Name {
	case PgBackRestDaliboPlugin:
		if p.Parameters[daliboStanzaRef] == "" {
			return PgBackRestSource{}, false
		}
		return PgBackRestSource{Plugin: p.Name, Object: p.Parameters[daliboStanzaRef], PluginConfig: p.Parameters[daliboPluginConfigRef]}, true
	case PgBackRestOperaPlugin:
		if p.Parameters[operaObjectName] == "" {
			return PgBackRestSource{}, false
		}
		stanza := p.Parameters[operaStanza]
		if stanza == "" {
			stanza = cluster
		}
		return PgBackRestSource{Plugin: p.Name, Object: p.Parameters[operaObjectName], Stanza: stanza}, true
	}
	return PgBackRestSource{}, false
}

// DrillName is <prefix><source>-<yymmddhhmm> (UTC), the source part
// shortened so the whole fits MaxClusterName.
func DrillName(prefix, source string, now time.Time) string {
	stamp := "-" + now.UTC().Format("0601021504")
	room := MaxClusterName - len(prefix) - len(stamp)
	if room < 1 {
		room = 1
	}
	if len(source) > room {
		source = strings.TrimRight(source[:room], "-.")
	}
	return prefix + source + stamp
}

// DrillResources is the drill instance's configured shape; empty fields
// keep the source's.
type DrillResources struct {
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
	StorageSize, StorageClass                        string
}

// The values of DrillServerCA.Mode (RESTORE_DRILL_SERVER_CA).
const (
	DrillServerCAOwn    = "own"
	DrillServerCASource = "source"
	DrillServerCASecret = "secret"
)

// DrillServerCA is the CA that signs a drill's server certificate, the
// one the console verifies the drill's /pg/status against:
//
//   - own (the default, and the zero value): CloudNativePG creates the
//     drill's <drill>-ca. Only a bundle someone keeps after the drill
//     exists can hold it: the operator's instance CA mirror of a
//     PgToolBoxConsole does; an admin-filled bundle (the pgtools chart) cannot.
//   - source: spec.certificates.serverCASecret names the source's server
//     CA Secret, already in an admin-filled bundle since the console
//     verifies the source with it. CloudNativePG reads that Secret only
//     (no owner reference, no renewal: a user-provided CA is never
//     touched) and signs the drill's server certificate with its key.
//   - secret: the same with a CA Secret the admin provides in the drill
//     namespace, for drills outside the source's namespace.
//
// The console reads none of these Secrets. The drill's client CA (what
// signs the streaming_replica and superuser client certificates) stays
// its own <drill>-ca in every mode.
type DrillServerCA struct {
	Mode   string
	Secret string
}

// ErrDrillServerCA is returned when the source-CA mode cannot sign a
// drill of a Cluster; the offer says why before the request.
var ErrDrillServerCA = errors.New("restore drill: the drill cannot be signed by the source's server CA")

// DrillServerCASecretFor is the CA Secret a drill of source in namespace
// is signed with: empty for "own", or why the "source" mode cannot apply
// (the console could not verify the drill: its check would time out).
func DrillServerCASecretFor(source *cnpgv1.Cluster, namespace string, ca DrillServerCA) (secret, why string) {
	if ca.Mode == DrillServerCASecret {
		return ca.Secret, ""
	}
	if ca.Mode != DrillServerCASource {
		return "", ""
	}
	if namespace != source.Namespace {
		return "", "the drill runs in " + namespace + " and CloudNativePG reads a Cluster's CA Secret in its own namespace; the source's is in " + source.Namespace
	}
	if source.Spec.Certificates != nil && source.Spec.Certificates.ServerTLSSecret != "" {
		return "", "the source's server certificate is user-provided (spec.certificates.serverTLSSecret), so its CA Secret need not hold the key a drill's certificate would be signed with"
	}
	if name := source.Status.Certificates.ServerCASecret; name != "" {
		return name, ""
	}
	if source.Spec.Certificates != nil && source.Spec.Certificates.ServerCASecret != "" {
		return source.Spec.Certificates.ServerCASecret, ""
	}
	return source.Name + "-ca", ""
}

// DrillRequest is everything DrillCluster builds from.
type DrillRequest struct {
	Source    *cnpgv1.Cluster
	Recovery  RecoverySource
	Namespace string
	Name      string
	Meta      DrillMeta
	Resources DrillResources
	// ExtraLabels are labels copied from the source so the drill stays
	// inside the console's CLUSTER_SELECTOR; reserved keys are refused.
	ExtraLabels map[string]string
	// ServerCA is the CA that signs the drill's server certificate.
	ServerCA DrillServerCA
}

// DrillCluster is the pinned drill Cluster: one instance bootstrapped by
// recovery from the source's archive, no WAL archiver, no backup
// configuration, no plugin, no replica role, so it cannot write into the
// repository it is testing (CheckDrillCluster enforces it). Image,
// PostgreSQL parameters (recovery refuses lower max_connections and
// friends than the primary had), tablespaces and storage follow the
// source unless configured.
func DrillCluster(r DrillRequest, keys [][]byte) (*cnpgv1.Cluster, error) {
	if len(keys) == 0 {
		return nil, errors.New("restore drill: no seal key")
	}
	src := r.Source
	labels := map[string]string{}
	for k, v := range r.ExtraLabels {
		if strings.HasPrefix(k, "pgtoolbox.fyannk.dev/") || strings.HasPrefix(k, "cnpg.io/") {
			continue
		}
		labels[k] = v
	}
	labels[LabelRestoreDrillOf] = src.Name
	labels[LabelRestoreDrillSourceNamespace] = src.Namespace
	labels[LabelDrillOwner] = r.Meta.Owner
	r.Meta.V = 1
	r.Meta.Source, r.Meta.SourceNamespace = src.Name, src.Namespace
	r.Meta.SourceUID = string(src.UID)
	record, err := json.Marshal(r.Meta)
	if err != nil {
		return nil, fmt.Errorf("restore drill: record: %w", err)
	}
	c := &cnpgv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: cnpgv1.SchemeGroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.Namespace, Name: r.Name, Labels: labels,
			Annotations: map[string]string{
				AnnotationRestoreDrill:     string(record),
				AnnotationRestoreDrillSeal: seal(keys[0], r.Namespace, r.Name, string(record)),
			},
		},
		Spec: cnpgv1.ClusterSpec{
			Description: "Restore drill of " + src.Namespace + "/" + src.Name + " (pgToolBox console); deleted after its check",
			Instances:   1,
			ImageName:   src.Spec.ImageName,
			PostgresUID: src.Spec.PostgresUID, PostgresGID: src.Spec.PostgresGID,
			ImagePullSecrets:     src.Spec.ImagePullSecrets,
			Bootstrap:            &cnpgv1.BootstrapConfiguration{Recovery: &cnpgv1.BootstrapRecovery{Source: DrillSourceName}},
			StorageConfiguration: drillStorage(src.Spec.StorageConfiguration, r.Resources.StorageSize, r.Resources.StorageClass),
			Resources:            drillResources(src.Spec.Resources, r.Resources),
		},
	}
	// A catalog reference resolves in the source's namespace only; the
	// image the operator resolved for the source is what was backed up.
	if src.Status.Image != "" {
		c.Spec.ImageName = src.Status.Image
	} else if c.Spec.ImageName == "" && src.Spec.ImageCatalogRef != nil && r.Namespace == src.Namespace {
		c.Spec.ImageCatalogRef = src.Spec.ImageCatalogRef.DeepCopy()
	}
	if src.Spec.WalStorage != nil {
		ws := drillStorage(*src.Spec.WalStorage, "", r.Resources.StorageClass)
		c.Spec.WalStorage = &ws
	}
	for _, ts := range src.Spec.Tablespaces {
		ts = *ts.DeepCopy()
		ts.Storage = drillStorage(ts.Storage, "", r.Resources.StorageClass)
		c.Spec.Tablespaces = append(c.Spec.Tablespaces, ts)
	}
	params := maps.Clone(src.Spec.PostgresConfiguration.Parameters)
	if params == nil {
		params = map[string]string{}
	}
	maps.Copy(params, DrillParameters)
	// The drill's own labels reach its Pods and its recovery Job only
	// through inheritedMetadata: the drill NetworkPolicy selects them.
	c.Spec.InheritedMetadata = &cnpgv1.EmbeddedObjectMetadata{Labels: map[string]string{
		LabelRestoreDrillOf:              src.Name,
		LabelRestoreDrillSourceNamespace: src.Namespace,
		LabelDrillOwner:                  r.Meta.Owner,
	}}
	c.Spec.PostgresConfiguration = cnpgv1.PostgresConfiguration{
		Parameters:          params,
		AdditionalLibraries: append([]string(nil), src.Spec.PostgresConfiguration.AdditionalLibraries...),
		Extensions:          append([]cnpgv1.ExtensionConfiguration(nil), src.Spec.PostgresConfiguration.Extensions...),
	}
	ext := cnpgv1.ExternalCluster{Name: DrillSourceName}
	if pb := r.Recovery.PgBackRest; pb != nil {
		// The same plugin and the parameters that name the source's
		// repository; the drill itself has no spec.plugins, so nothing
		// archives (CNPG calls the WAL archiver of spec.plugins only).
		if pb.Target == "" {
			return nil, errors.New("restore drill: no pgBackRest backup label to restore")
		}
		ext.PluginConfiguration = &cnpgv1.PluginConfiguration{Name: pb.Plugin, Parameters: pb.Parameters()}
		c.Spec.Bootstrap.Recovery.RecoveryTarget = &cnpgv1.RecoveryTarget{BackupID: pb.Target}
	} else if r.Recovery.Plugin {
		ext.PluginConfiguration = &cnpgv1.PluginConfiguration{
			Name:       BarmanCloudPlugin,
			Parameters: map[string]string{"barmanObjectName": r.Recovery.ObjectStore, "serverName": r.Recovery.ServerName},
		}
	} else {
		if r.Recovery.InTree == nil {
			return nil, ErrNoRecoverySource
		}
		ext.BarmanObjectStore = r.Recovery.InTree.DeepCopy()
	}
	c.Spec.ExternalClusters = []cnpgv1.ExternalCluster{ext}
	caSecret, why := DrillServerCASecretFor(src, r.Namespace, r.ServerCA)
	if why != "" {
		return nil, fmt.Errorf("%w: %s", ErrDrillServerCA, why)
	}
	if caSecret != "" {
		// The server CA only: the client CA, the replication and the
		// server certificates stay the drill's own, generated.
		c.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: caSecret}
	}
	if err := CheckDrillCluster(c); err != nil {
		return nil, err
	}
	return c, nil
}

func drillStorage(src cnpgv1.StorageConfiguration, size, class string) cnpgv1.StorageConfiguration {
	out := cnpgv1.StorageConfiguration{Size: src.Size}
	if src.StorageClass != nil {
		sc := *src.StorageClass
		out.StorageClass = &sc
	}
	if src.PersistentVolumeClaimTemplate != nil {
		t := src.PersistentVolumeClaimTemplate.DeepCopy()
		// The template's data source, selector and volume belong to the
		// source: a selector could bind the drill to a pre-provisioned
		// volume meant for someone else.
		t.DataSource, t.DataSourceRef, t.VolumeName, t.Selector = nil, nil, "", nil
		out.PersistentVolumeClaimTemplate = t
	}
	if size != "" {
		out.Size = size
		if out.PersistentVolumeClaimTemplate != nil {
			delete(out.PersistentVolumeClaimTemplate.Resources.Requests, corev1.ResourceStorage)
		}
	}
	if class != "" {
		c := class
		out.StorageClass = &c
		if out.PersistentVolumeClaimTemplate != nil {
			out.PersistentVolumeClaimTemplate.StorageClassName = nil
		}
	}
	return out
}

func drillResources(src corev1.ResourceRequirements, r DrillResources) corev1.ResourceRequirements {
	out := *src.DeepCopy()
	set := func(list *corev1.ResourceList, name corev1.ResourceName, value string) {
		if value == "" {
			return
		}
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = resource.MustParse(value)
	}
	set(&out.Requests, corev1.ResourceCPU, r.CPURequest)
	set(&out.Requests, corev1.ResourceMemory, r.MemoryRequest)
	set(&out.Limits, corev1.ResourceCPU, r.CPULimit)
	set(&out.Limits, corev1.ResourceMemory, r.MemoryLimit)
	return out
}

// ErrDrillWrites is the invariant's error: the object could write into
// a backup repository.
var ErrDrillWrites = errors.New("restore drill: the Cluster could write into a backup repository")

// CheckDrillCluster is the invariant every drill Cluster must keep before
// it is sent: exactly one instance bootstrapped by recovery from one
// external source, and nothing that archives WAL or takes backups — no
// spec.backup, no spec.plugins (the WAL archiver lives there), no replica
// role, no in-tree archiver section of the source used for anything but
// reading. Without it a drill could archive into the source's server
// path, the classic way a restore test corrupts the backups it tests.
func CheckDrillCluster(c *cnpgv1.Cluster) error {
	s := c.Spec
	var why []string
	if s.Backup != nil {
		why = append(why, "spec.backup is set")
	}
	if len(s.Plugins) > 0 {
		why = append(why, "spec.plugins is set")
	}
	if s.ReplicaCluster != nil {
		why = append(why, "spec.replica is set")
	}
	if s.Instances != 1 {
		why = append(why, "instances is not 1")
	}
	if s.ServiceAccountTemplate != nil {
		why = append(why, "spec.serviceAccountTemplate is set (it can lend the drill the source's cloud role)")
	}
	if cs := s.Certificates; cs != nil && (cs.ServerTLSSecret != "" || cs.ClientCASecret != "" || cs.ReplicationTLSSecret != "" || len(cs.ServerAltDNSNames) > 0) {
		why = append(why, "spec.certificates carries more than serverCASecret (the drill's client CA and certificates are its own)")
	}
	for k, v := range DrillParameters {
		if s.PostgresConfiguration.Parameters[k] != v {
			why = append(why, "postgresql.parameters."+k+" is not "+v)
		}
	}
	if im := s.InheritedMetadata; im == nil || im.Labels[LabelDrillOwner] == "" ||
		im.Labels[LabelDrillOwner] != c.Labels[LabelDrillOwner] || im.Labels[LabelRestoreDrillOf] != c.Labels[LabelRestoreDrillOf] {
		why = append(why, "the drill labels are not inherited by its Pods (the drill NetworkPolicy would not select them)")
	}
	if s.Bootstrap == nil || s.Bootstrap.Recovery == nil || s.Bootstrap.Recovery.Source != DrillSourceName ||
		s.Bootstrap.InitDB != nil || s.Bootstrap.PgBaseBackup != nil {
		why = append(why, "the bootstrap is not a recovery from "+DrillSourceName)
	}
	if len(s.ExternalClusters) != 1 || s.ExternalClusters[0].Name != DrillSourceName {
		why = append(why, "externalClusters is not exactly "+DrillSourceName)
	} else if ext := s.ExternalClusters[0]; ext.PluginConfiguration != nil {
		if ext.PluginConfiguration.IsWALArchiver != nil && *ext.PluginConfiguration.IsWALArchiver {
			why = append(why, "the recovery plugin is marked as WAL archiver")
		}
		if ext.BarmanObjectStore != nil {
			why = append(why, "the source has both a plugin and barmanObjectStore")
		}
		why = append(why, checkPgBackRestSource(s, *ext.PluginConfiguration)...)
	} else if ext.BarmanObjectStore == nil {
		why = append(why, "the source has neither a plugin nor barmanObjectStore")
	}
	if len(why) > 0 {
		return fmt.Errorf("%w: %s", ErrDrillWrites, strings.Join(why, "; "))
	}
	return nil
}

// checkPgBackRestSource is the pgBackRest part of the invariant. The
// recovery source carries only the parameters that name the source's
// repository (anything else could configure the plugin to do more than
// read), Opera's stanza is explicit (its default is the recovering
// Cluster's name), and no spec.plugins entry names the source's Stanza
// or Archive: that entry would archive the drill's WAL into the source's
// stanza. spec.plugins is refused whole above; this says why for the
// worst case.
func checkPgBackRestSource(s cnpgv1.ClusterSpec, ext cnpgv1.PluginConfiguration) []string {
	var allowed []string
	var object string
	switch ext.Name {
	case PgBackRestDaliboPlugin:
		allowed, object = []string{daliboStanzaRef, daliboPluginConfigRef}, ext.Parameters[daliboStanzaRef]
		if object == "" {
			return []string{"the pgBackRest (Dalibo) recovery source names no stanzaRef"}
		}
	case PgBackRestOperaPlugin:
		allowed, object = []string{operaObjectName, operaStanza}, ext.Parameters[operaObjectName]
		if object == "" || ext.Parameters[operaStanza] == "" {
			return []string{"the pgBackRest (Opera) recovery source does not name both pgbackrestObjectName and stanza"}
		}
	default:
		return nil
	}
	var why []string
	for k := range ext.Parameters {
		if !slices.Contains(allowed, k) {
			why = append(why, "the pgBackRest recovery source carries parameter "+k+" (only "+strings.Join(allowed, ", ")+" are copied)")
		}
	}
	slices.Sort(why)
	if s.Bootstrap == nil || s.Bootstrap.Recovery == nil || s.Bootstrap.Recovery.RecoveryTarget == nil || s.Bootstrap.Recovery.RecoveryTarget.BackupID == "" {
		why = append(why, "the pgBackRest recovery names no backup label (recoveryTarget.backupID)")
	}
	for _, p := range s.Plugins {
		if p.Name == ext.Name && (p.Parameters[daliboStanzaRef] == object || p.Parameters[operaObjectName] == object) {
			why = append(why, "spec.plugins names the source's "+object+": the drill would archive into the source's stanza")
		}
	}
	return why
}

// seal is the HMAC-SHA256 of the drill's namespace, name and record.
func seal(key []byte, namespace, name, record string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("pgtoolbox-console-restore-drill/v1\n" + namespace + "\n" + name + "\n" + record))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ErrNotOurDrill is returned for a Cluster this console did not create
// as a drill: missing or mismatched labels, another owner, or a seal that
// does not verify.
var ErrNotOurDrill = errors.New("not a restore drill created by this console")

// VerifyDrill returns the creation record of a drill this console
// created (owner, every key tried for rotation), or ErrNotOurDrill.
func VerifyDrill(c *cnpgv1.Cluster, owner string, keys [][]byte) (DrillMeta, error) {
	if c == nil || owner == "" || c.Labels[LabelDrillOwner] != owner {
		return DrillMeta{}, ErrNotOurDrill
	}
	record, sealed := c.Annotations[AnnotationRestoreDrill], c.Annotations[AnnotationRestoreDrillSeal]
	if record == "" || sealed == "" || len(record) > 4096 {
		return DrillMeta{}, ErrNotOurDrill
	}
	ok := false
	for _, k := range keys {
		if hmac.Equal([]byte(seal(k, c.Namespace, c.Name, record)), []byte(sealed)) {
			ok = true
			break
		}
	}
	if !ok {
		return DrillMeta{}, ErrNotOurDrill
	}
	var m DrillMeta
	if err := json.Unmarshal([]byte(record), &m); err != nil || m.V != 1 || m.Owner != owner ||
		c.Labels[LabelRestoreDrillOf] != m.Source || c.Labels[LabelRestoreDrillSourceNamespace] != m.SourceNamespace {
		return DrillMeta{}, ErrNotOurDrill
	}
	// The API server sets creationTimestamp: a sealed record copied onto a
	// Cluster created later (the drill deleted, its name reused) is not
	// accepted.
	created := c.CreationTimestamp.Time
	if created.IsZero() || created.Before(m.StartedAt.Add(-DrillCreationSkewBefore)) || created.After(m.StartedAt.Add(DrillCreationSkewAfter)) {
		return DrillMeta{}, ErrNotOurDrill
	}
	return m, nil
}

// CreateDrillCluster creates the drill Cluster as the user, after
// re-checking the invariant on the exact object sent.
func (w *UserWriter) CreateDrillCluster(ctx context.Context, c *cnpgv1.Cluster) (types.UID, error) {
	if err := CheckDrillCluster(c); err != nil {
		return "", &WriteError{Status: 500, Text: "creating the drill Cluster: refused by the console's own invariant", cause: err}
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.c.Create(ctx, c); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return "", &WriteError{Status: 409, Text: "creating the drill Cluster: a Cluster of that name exists; retry in a minute", cause: err}
		}
		return "", classify("creating the drill Cluster", err)
	}
	return c.UID, nil
}

// DrillReaper deletes drill Clusters with the console's own
// ServiceAccount (the opt-in teardown Role). It never deletes anything
// VerifyDrill does not accept.
type DrillReaper struct {
	c       client.Client
	owner   string
	keys    [][]byte
	timeout time.Duration
}

// NewDrillReaper wraps a client from NewDrillReaperClient.
func NewDrillReaper(c client.Client, owner string, keys [][]byte, timeout time.Duration) *DrillReaper {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &DrillReaper{c: c, owner: owner, keys: keys, timeout: timeout}
}

// Owner is the console identity the reaper deletes for.
func (r *DrillReaper) Owner() string { return r.owner }

// Verify is VerifyDrill with the reaper's identity and keys.
func (r *DrillReaper) Verify(c *cnpgv1.Cluster) (DrillMeta, error) {
	return VerifyDrill(c, r.owner, r.keys)
}

// Teardown deletes the drill c after verifying it, preconditioned on its
// UID (a Cluster recreated under the same name in between is not
// touched). The PVCs follow through their owner reference to the Cluster
// (CloudNativePG sets it), so the console deletes nothing else. A drill
// already gone is success.
func (r *DrillReaper) Teardown(ctx context.Context, c *cnpgv1.Cluster) error {
	if _, err := r.Verify(c); err != nil {
		return err
	}
	if c.UID == "" {
		return fmt.Errorf("%w: no UID to precondition the delete on", ErrNotOurDrill)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	uid := c.UID
	target := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Name}}
	err := r.c.Delete(ctx, target, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationBackground))
	switch {
	case err == nil || apierrors.IsNotFound(err):
		return nil
	case apierrors.IsForbidden(err):
		return &WriteError{Status: 403, Text: "deleting the drill Cluster: the console's ServiceAccount may not delete Clusters in " +
			c.Namespace + " (the restore-drill teardown Role is missing)", cause: err}
	}
	return classify("deleting the drill Cluster", err)
}

// NewDrillReaperClient builds the reaper's client over the console's own
// configuration with a static REST mapping (Clusters only).
func NewDrillReaperClient(cfg *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("ops: drill scheme: %w", err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(cnpgv1.SchemeGroupVersion.WithKind("Cluster"), meta.RESTScopeNamespace)
	c, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, fmt.Errorf("ops: drill reaper client: %w", err)
	}
	return c, nil
}

// StoreFacts are what a drill compares between the source's archive and
// the one it would read: where, and with which kind of credentials.
type StoreFacts struct {
	DestinationPath string
	EndpointURL     string
	ServerName      string
	// WorkloadIdentity names the ambient-credential option in use
	// (inheritFromIAMRole, gkeEnvironment, inheritFromAzureAD), "" for
	// credentials from Secrets.
	WorkloadIdentity string
}

// FactsOf reads a barman object store configuration (in-tree, or an
// ObjectStore's spec.configuration).
func FactsOf(cfg *cnpgv1.BarmanObjectStoreConfiguration) StoreFacts {
	if cfg == nil {
		return StoreFacts{}
	}
	f := StoreFacts{
		DestinationPath: strings.TrimRight(cfg.DestinationPath, "/"),
		EndpointURL:     strings.TrimRight(cfg.EndpointURL, "/"),
		ServerName:      cfg.ServerName,
	}
	switch c := cfg.BarmanCredentials; {
	case c.AWS != nil && c.AWS.InheritFromIAMRole:
		f.WorkloadIdentity = "inheritFromIAMRole"
	case c.Google != nil && c.Google.GKEEnvironment:
		f.WorkloadIdentity = "gkeEnvironment"
	case c.Azure != nil && c.Azure.InheritFromAzureAD:
		f.WorkloadIdentity = "inheritFromAzureAD"
	}
	return f
}

// ObjectStoreFacts reads a barman-cloud ObjectStore (unstructured: the
// console does not link the plugin's API) through its spec.configuration.
func ObjectStoreFacts(store map[string]any) (StoreFacts, error) {
	spec, _ := store["spec"].(map[string]any)
	raw, ok := spec["configuration"]
	if !ok {
		return StoreFacts{}, errors.New("the ObjectStore has no spec.configuration")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return StoreFacts{}, err
	}
	cfg := &cnpgv1.BarmanObjectStoreConfiguration{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return StoreFacts{}, fmt.Errorf("the ObjectStore's spec.configuration: %w", err)
	}
	return FactsOf(cfg), nil
}
