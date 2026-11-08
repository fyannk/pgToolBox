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

package console

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// The instance CA mirror. The console
// verifies each instance's /pg/status against the server CAs of its
// Clusters, read from a mounted ConfigMap. CloudNativePG's self-signed CA
// is valid 90 days and renewed 7 days before it expires, so a copy made by
// hand stops matching within 90 days; the operator, which already reads
// Secrets and is not exposed, keeps the copy instead:
//
//   - for every Cluster of the namespace the PgToolBoxConsole's clusterSelector
//     matches, it reads the server CA Secret (status, else spec
//     certificates.serverCASecret, else <cluster>-ca) and takes its ca.crt
//     and nothing else: ca.key never leaves the Secret, and only the
//     CERTIFICATE PEM blocks of ca.crt are re-encoded into the bundle;
//   - identical CAs (one user-managed serverCASecret shared by several
//     Clusters) appear once; the order is the Clusters' names, so the
//     bundle is byte-stable and a no-op loop writes nothing;
//   - the result is <name>-instance-ca, applied like every other child.
//
// The console still reads no Secret and its ServiceAccount gains nothing.
// spec.instanceCABundleRef turns all of this off: the console mounts that
// ConfigMap and the operator neither reads nor writes it.

const (
	// caKey is the only key read from a server CA Secret, and the bundle's
	// one key (the console's INSTANCE_STATUS_CA_FILE is <mount>/ca.crt).
	caKey = "ca.crt"
	// maxBundleBytes keeps the bundle under the 1 MiB a ConfigMap holds,
	// with room for its metadata: several hundred distinct CAs (a
	// CloudNativePG ECDSA CA is about 700 bytes of PEM).
	maxBundleBytes = 900 << 10
)

// indexServerCASecret indexes Clusters by the name of their server CA
// Secret, so a Secret event finds the Clusters (and so the consoles) it
// concerns without a scan.
const indexServerCASecret = "console.serverCASecret"

// serverCASecretName is the Secret holding a Cluster's server CA: what
// CloudNativePG resolved (status), else what the spec asks for, else its
// default <cluster>-ca.
func serverCASecretName(cluster *cnpgv1.Cluster) string {
	if name := cluster.Status.Certificates.ServerCASecret; name != "" {
		return name
	}
	if cluster.Spec.Certificates != nil && cluster.Spec.Certificates.ServerCASecret != "" {
		return cluster.Spec.Certificates.ServerCASecret
	}
	return cluster.Name + "-ca"
}

// clusterCA is one selected Cluster and what was read from its server CA
// Secret: the ca.crt bytes, or why there are none.
type clusterCA struct {
	Cluster string
	Secret  string
	CACert  []byte
	Missing pgtoolboxv1alpha1.Reason
}

// instanceCABundle is what buildInstanceCABundle renders: the ConfigMap and
// the facts status reports.
type instanceCABundle struct {
	ConfigMap *corev1.ConfigMap
	// Clusters counts the Clusters whose CA is in the bundle.
	Clusters int
	// Distinct counts the CA certificates in the bundle.
	Distinct int
	// Missing are the Clusters left out, by name.
	Missing []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing
}

// buildInstanceCABundle renders the mirror from the Clusters' CAs, sorted
// by Cluster name. Only CERTIFICATE blocks that parse are kept, each
// re-encoded from its DER: whatever else a ca.crt might carry (a private
// key pasted by mistake, comments) is dropped, never copied.
func buildInstanceCABundle(c *pgtoolboxv1alpha1.PgToolBoxConsole, cas []clusterCA) instanceCABundle {
	return buildBoundedInstanceCABundle(c, cas, maxBundleBytes)
}

// buildBoundedInstanceCABundle is buildInstanceCABundle with the size bound
// as a parameter, for the tests.
func buildBoundedInstanceCABundle(c *pgtoolboxv1alpha1.PgToolBoxConsole, cas []clusterCA, limit int) instanceCABundle {
	sorted := append([]clusterCA(nil), cas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cluster < sorted[j].Cluster })
	out := instanceCABundle{}
	seen := map[string]bool{}
	var bundle bytes.Buffer
	for _, ca := range sorted {
		missing := func(reason pgtoolboxv1alpha1.Reason) {
			out.Missing = append(out.Missing, pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{Cluster: ca.Cluster, Secret: ca.Secret, Reason: reason})
		}
		if ca.Missing != "" {
			missing(ca.Missing)
			continue
		}
		certificates := certificateBlocks(ca.CACert)
		if len(certificates) == 0 {
			missing(pgtoolboxv1alpha1.ReasonInstanceCAInvalid)
			continue
		}
		var added bytes.Buffer
		var addedKeys []string
		for _, der := range certificates {
			key := string(der)
			if seen[key] {
				continue
			}
			seen[key] = true
			addedKeys = append(addedKeys, key)
			_ = pem.Encode(&added, &pem.Block{Type: "CERTIFICATE", Bytes: der}) // a bytes.Buffer write cannot fail
		}
		if bundle.Len()+added.Len() > limit {
			for _, key := range addedKeys {
				delete(seen, key)
			}
			missing(pgtoolboxv1alpha1.ReasonInstanceCABundleFull)
			continue
		}
		bundle.Write(added.Bytes())
		out.Distinct += len(addedKeys)
		out.Clusters++
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(c, c.InstanceCAMirrorName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Data:       map[string]string{caKey: bundle.String()},
	}
	specs.SetHash(cm, specs.Hash(cm.Data))
	out.ConfigMap = cm
	return out
}

// certificateBlocks returns the DER of every CERTIFICATE PEM block of raw
// that parses as an X.509 certificate, in order.
func certificateBlocks(raw []byte) [][]byte {
	var out [][]byte
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			continue
		}
		out = append(out, block.Bytes)
	}
}

// collectInstanceCAs reads the server CA of every Cluster of the namespace
// the PgToolBoxConsole selects. Clusters come from the cache (the manager already
// holds them); each Secret is one direct read, because Secret content is
// never cached. The whole Secret transits through memory (the API has no
// per-key read) and only ca.crt is kept. Only a failed read other than
// NotFound is an error.
func collectInstanceCAs(ctx context.Context, cached, secrets client.Reader, c *pgtoolboxv1alpha1.PgToolBoxConsole) ([]clusterCA, error) {
	var clusters cnpgv1.ClusterList
	if err := cached.List(ctx, &clusters, client.InNamespace(c.Namespace)); err != nil {
		return nil, fmt.Errorf("list Clusters: %w", err)
	}
	selector := clusterSelector(c)
	cas := make([]clusterCA, 0, len(clusters.Items))
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if !selector.Matches(labels.Set(cluster.Labels)) {
			continue
		}
		ca := clusterCA{Cluster: cluster.Name, Secret: serverCASecretName(cluster)}
		secret := &corev1.Secret{}
		err := secrets.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ca.Secret}, secret)
		switch {
		case apierrors.IsNotFound(err):
			ca.Missing = pgtoolboxv1alpha1.ReasonSecretNotFound
		case err != nil:
			return nil, fmt.Errorf("read the server CA Secret %s of Cluster %s: %w", ca.Secret, cluster.Name, err)
		case len(secret.Data[caKey]) == 0:
			ca.Missing = pgtoolboxv1alpha1.ReasonSecretKeyMissing
		default:
			ca.CACert = secret.Data[caKey]
		}
		cas = append(cas, ca)
	}
	return cas, nil
}

// clusterSelector is the PgToolBoxConsole's clusterSelector, the one the console
// receives as CLUSTER_SELECTOR: a selector that does not parse is not
// rendered there either (consoleEnv), so the console then shows every
// Cluster and the bundle covers every Cluster.
func clusterSelector(c *pgtoolboxv1alpha1.PgToolBoxConsole) labels.Selector {
	if c.Spec.ClusterSelector == nil {
		return labels.Everything()
	}
	selector, err := metav1.LabelSelectorAsSelector(c.Spec.ClusterSelector)
	if err != nil {
		return labels.Everything()
	}
	return selector
}

// reconcileInstanceCA keeps the mirror and reports it in
// status.instanceCABundle and the advisory InstanceCABundleComplete. A
// missing CA never fails the loop: the console shows that Cluster as "not
// verifiable" until its CA appears.
func (r *Reconciler) reconcileInstanceCA(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	if !console.InstanceCAMirrored() {
		console.Status.InstanceCABundle = &pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCABundleStatus{
			ConfigMap: console.InstanceCABundleName(), KeptBy: pgtoolboxv1alpha1.InstanceCAReferenced,
		}
		meta.RemoveStatusCondition(&console.Status.Conditions, pgtoolboxv1alpha1.ConditionInstanceCABundleComplete)
		// A mirror a previous spec had is left in place, unmounted and no
		// longer updated, as the seeded lifecycle ConfigMap is when
		// lifecycleConfigMapRef is set: it holds public certificates only,
		// goes with the PgToolBoxConsole (owner reference), and deleting it would
		// take a ConfigMap delete verb the operator does not otherwise need.
		return nil
	}
	cas, err := collectInstanceCAs(ctx, r.Client, r.APIReader, console)
	if err != nil {
		return err
	}
	bundle := buildInstanceCABundle(console, cas)
	status := &pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCABundleStatus{
		ConfigMap: bundle.ConfigMap.Name, KeptBy: pgtoolboxv1alpha1.InstanceCAKeptByOperator,
		Clusters: int32(min(bundle.Clusters, 1<<30)), // #nosec G115 -- bounded above
	}
	status.Missing = bundle.Missing[:min(len(bundle.Missing), pgtoolboxv1alpha1.MaxInstanceCAMissing)]
	if len(status.Missing) == 0 {
		status.Missing = nil
	}
	console.Status.InstanceCABundle = status
	if err := r.applier().Apply(ctx, bundle.ConfigMap); err != nil {
		var foreign *reconcile.NotOwnedError
		if !errors.As(err, &foreign) {
			return err
		}
		// Someone else's ConfigMap has the bundle's name (typically the
		// one an admin made by hand for an earlier default): the console
		// mounts it as it is; the operator says so and keeps the console
		// running rather than stalling it.
		status.Clusters, status.Missing = 0, nil
		setCondition(console, pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned,
			fmt.Sprintf("ConfigMap %s exists and is not this console's, so the operator does not keep it current: delete it to let the operator mirror the Clusters' CAs, or name it in spec.instanceCABundleRef to keep it yourself", bundle.ConfigMap.Name))
		return nil
	}
	conditionStatus, reason, message := instanceCACondition(bundle)
	setCondition(console, pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, conditionStatus, reason, message)
	return nil
}

// instanceCACondition is InstanceCABundleComplete for a rendered bundle.
func instanceCACondition(bundle instanceCABundle) (metav1.ConditionStatus, pgtoolboxv1alpha1.Reason, string) {
	name := bundle.ConfigMap.Name
	total := bundle.Clusters + len(bundle.Missing)
	switch {
	case total == 0:
		return metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, fmt.Sprintf("no Cluster selected; ConfigMap %s is empty", name)
	case len(bundle.Missing) == 0:
		return metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			fmt.Sprintf("the server CAs of all %d selected Clusters are in ConfigMap %s (%d distinct)", total, name, bundle.Distinct)
	}
	parts := make([]string, 0, len(bundle.Missing))
	for _, m := range bundle.Missing {
		parts = append(parts, fmt.Sprintf("%s (%s, Secret %s)", m.Cluster, m.Reason, m.Secret))
	}
	return metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonInstanceCAMissing,
		fmt.Sprintf("%d of %d selected Clusters have no CA in ConfigMap %s and read as not verifiable: %s",
			len(bundle.Missing), total, name, strings.Join(parts, ", "))
}
