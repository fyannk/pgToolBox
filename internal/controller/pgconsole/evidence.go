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

package pgconsole

import (
	"context"
	"fmt"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/evidence"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// barmanPluginName is the CNPG plugin the Barman Cloud Plugin registers as,
// and barmanObjectParameter the stanza parameter naming its ObjectStore.
const (
	barmanPluginName      = "barman-cloud.cloudnative-pg.io"
	barmanObjectParameter = "barmanObjectName"
)

var objectStoreGVK = schema.GroupVersionKind{
	Group:   "barmancloud.cnpg.io",
	Version: "v1",
	Kind:    "ObjectStore",
}

// secretKeyRef names one key of one same-namespace Secret, read from the
// ObjectStore CR — never derived from a convention.
type secretKeyRef struct {
	Name string
	Key  string
}

// evidenceComposition is everything the Pod composition needs, resolved from
// the Cluster and its ObjectStore. It deliberately carries plain values:
// only this file imports the Barman types, the way a provider adapter
// confines its upstream.
type evidenceComposition struct {
	ObjectStoreName string
	ClusterUID      string
	ServerName      string
	// DestinationPath is the ObjectStore's original string, injected
	// verbatim; Destination is its parsed form, used for the fingerprint.
	DestinationPath string
	Destination     evidence.Destination
	EndpointURL     string
	EndpointCA      *secretKeyRef
	AccessKeyID     secretKeyRef
	SecretAccessKey secretKeyRef
	SessionToken    *secretKeyRef
	Fingerprint     string
}

// evidenceUnavailable is the typed "why not" for the
// RepositoryEvidenceReady condition. It is an expected outcome, not an
// error: a cluster without object-store backup simply has no evidence.
type evidenceUnavailable struct {
	reason  string
	message string
}

func (e *evidenceUnavailable) Error() string { return e.message }

func unavailable(reason, format string, args ...any) *evidenceUnavailable {
	return &evidenceUnavailable{reason: reason, message: fmt.Sprintf(format, args...)}
}

// evidenceEnabled reports whether the evidence sidecar is requested; the
// field defaults to false in the API.
func evidenceEnabled(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Evidence.Enabled != nil && *console.Spec.Evidence.Enabled
}

// reconcileEvidence resolves the composition, runs the token lifecycle, and
// publishes the outcome — status.evidence plus the RepositoryEvidenceReady
// condition. A nil composition with nil error is the degraded path: the
// Deployment is built without a sidecar and the condition names the gap.
func (r *Reconciler) reconcileEvidence(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*evidenceComposition, string, error) {
	composition, whyNot, err := r.resolveEvidence(ctx, console)
	if err != nil {
		return nil, "", err
	}
	if whyNot == nil && r.viewerImage(console) == "" {
		whyNot = unavailable(pgtoolboxv1alpha1.ReasonImageRequired,
			"cluster %s archives to ObjectStore %s, but no viewer image is configured "+
				"(spec.evidence.image or --default-objectstoreviewer-image)",
			console.Spec.CNPGClusterRef.Name, composition.ObjectStoreName)
	}
	if whyNot != nil {
		console.Status.Evidence = pgtoolboxv1alpha1.EvidenceStatus{}
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionRepositoryEvidenceReady,
			whyNot.reason,
			"%s",
			whyNot.message,
		)
		return nil, "", nil
	}

	tokenSecretName, err := r.reconcileEvidenceToken(ctx, console)
	if err != nil {
		return nil, "", err
	}
	console.Status.Evidence = pgtoolboxv1alpha1.EvidenceStatus{
		Enabled:         true,
		TokenSecretName: tokenSecretName,
	}
	conditions.MarkTrue(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionRepositoryEvidenceReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"evidence sidecar composed for ObjectStore %s, server %s",
		composition.ObjectStoreName,
		composition.ServerName,
	)
	return composition, tokenSecretName, nil
}

// resolveEvidence walks Cluster → plugin stanza → ObjectStore → credential
// references and computes the destination fingerprint. Reads go through
// APIReader: the foreign objects are consumed transiently on each reconcile,
// never cached, and the answer is republished into status — the same
// continuous-resolution discipline as registration credentials.
func (r *Reconciler) resolveEvidence(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*evidenceComposition, *evidenceUnavailable, error) {
	if !evidenceEnabled(console) {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonEvidenceDisabled,
			"evidence sidecar is disabled by spec.evidence.enabled"), nil
	}

	var cluster cnpgv1.Cluster
	clusterKey := client.ObjectKey{Namespace: console.Namespace, Name: console.Spec.CNPGClusterRef.Name}
	if err := r.APIReader.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, unavailable(pgtoolboxv1alpha1.ReasonObjectStoreNotFound,
				"target cluster %s was not found", clusterKey.Name), nil
		}
		return nil, nil, fmt.Errorf("read target cluster %s: %w", clusterKey.Name, err)
	}

	objectStoreName := barmanObjectStoreName(&cluster)
	if objectStoreName == "" {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonObjectStoreNotFound,
			"cluster %s declares no Barman Cloud Plugin object store", cluster.Name), nil
	}
	if !r.BarmanObjectStoreAvailable {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonObjectStoreNotFound,
			"the barmancloud.cnpg.io ObjectStore API is not served on this cluster"), nil
	}

	configuration, err := r.readObjectStoreConfiguration(ctx, console.Namespace, objectStoreName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, unavailable(pgtoolboxv1alpha1.ReasonObjectStoreNotFound,
				"ObjectStore %s referenced by cluster %s was not found", objectStoreName, cluster.Name), nil
		}
		return nil, nil, err
	}

	composition, whyNot := composeFromConfiguration(&cluster, objectStoreName, configuration)
	if whyNot != nil {
		return nil, whyNot, nil
	}
	return composition, nil, nil
}

// barmanObjectStoreName extracts the ObjectStore reference from the
// cluster's plugin stanza. An explicitly disabled plugin counts as absent.
func barmanObjectStoreName(cluster *cnpgv1.Cluster) string {
	for i := range cluster.Spec.Plugins {
		plugin := &cluster.Spec.Plugins[i]
		if plugin.Name != barmanPluginName {
			continue
		}
		if plugin.Enabled != nil && !*plugin.Enabled {
			return ""
		}
		return plugin.Parameters[barmanObjectParameter]
	}
	return ""
}

// readObjectStoreConfiguration reads the ObjectStore unstructured — the
// operator carries no scheme registration for a CRD that may be absent —
// and decodes only spec.configuration into the Barman configuration type
// already vendored through CloudNativePG.
func (r *Reconciler) readObjectStoreConfiguration(
	ctx context.Context,
	namespace, name string,
) (*barmanapi.BarmanObjectStoreConfiguration, error) {
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(objectStoreGVK)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, store); err != nil {
		return nil, err
	}
	rawConfiguration, found, err := unstructured.NestedMap(store.Object, "spec", "configuration")
	if err != nil || !found {
		return nil, fmt.Errorf("ObjectStore %s/%s carries no spec.configuration", namespace, name)
	}
	configuration := &barmanapi.BarmanObjectStoreConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawConfiguration, configuration); err != nil {
		return nil, fmt.Errorf("decode ObjectStore %s/%s configuration: %w", namespace, name, err)
	}
	return configuration, nil
}

// composeFromConfiguration maps the Barman configuration onto the initial
// contract profile: S3, static file credentials, one server. Every shape
// outside that profile degrades with a reason naming the gap — unknown
// never defaults to healthy.
func composeFromConfiguration(
	cluster *cnpgv1.Cluster,
	objectStoreName string,
	configuration *barmanapi.BarmanObjectStoreConfiguration,
) (*evidenceComposition, *evidenceUnavailable) {
	if configuration.Azure != nil || configuration.Google != nil {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s uses a non-S3 provider; the initial evidence profile is s3 only", objectStoreName)
	}
	aws := configuration.AWS
	if aws == nil {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s declares no s3Credentials", objectStoreName)
	}
	if aws.InheritFromIAMRole {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s inherits IAM credentials; the static-files profile needs explicit keys", objectStoreName)
	}
	if aws.AccessKeyIDReference == nil || aws.SecretAccessKeyReference == nil {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s names no static access key pair", objectStoreName)
	}

	destination, err := evidence.ParseDestination(configuration.DestinationPath)
	if err != nil {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s: %v", objectStoreName, err)
	}

	serverName := configuration.ServerName
	if serverName == "" {
		serverName = cluster.Name
	}
	fingerprint, err := evidence.Fingerprint(destination, configuration.EndpointURL, serverName)
	if err != nil {
		return nil, unavailable(pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode,
			"ObjectStore %s: destination rejected by the shared canonicalization: %v", objectStoreName, err)
	}

	composition := &evidenceComposition{
		ObjectStoreName: objectStoreName,
		ClusterUID:      string(cluster.UID),
		ServerName:      serverName,
		DestinationPath: configuration.DestinationPath,
		Destination:     destination,
		EndpointURL:     configuration.EndpointURL,
		AccessKeyID: secretKeyRef{
			Name: aws.AccessKeyIDReference.Name,
			Key:  aws.AccessKeyIDReference.Key,
		},
		SecretAccessKey: secretKeyRef{
			Name: aws.SecretAccessKeyReference.Name,
			Key:  aws.SecretAccessKeyReference.Key,
		},
		Fingerprint: fingerprint,
	}
	if aws.SessionToken != nil {
		composition.SessionToken = &secretKeyRef{Name: aws.SessionToken.Name, Key: aws.SessionToken.Key}
	}
	if configuration.EndpointCA != nil {
		composition.EndpointCA = &secretKeyRef{Name: configuration.EndpointCA.Name, Key: configuration.EndpointCA.Key}
	}
	return composition, nil
}
