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

package pgadmin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/exposure"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// aspects are the conditions Ready summarizes, in the order that decides
// Ready's reason. PgAdminEgressConfined is advisory and never among them.
var aspects = []string{
	pgtoolboxv1alpha1.ConditionConfigurationValid,
	pgtoolboxv1alpha1.ConditionWorkloadReady,
	pgtoolboxv1alpha1.ConditionExposureReady,
	pgtoolboxv1alpha1.ConditionServersSynced,
}

// Reconcile is the controller's loop: read, guard, run the concerns in
// order, and publish status on every exit path — the published status is
// the last write of the loop and Ready derives from the aspect conditions
// in that same write.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p pgtoolboxv1alpha1.PgAdmin
	if err := r.Client.Get(ctx, req.NamespacedName, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.pruneAccess(ctx, req.NamespacedName, nil)
		}
		return ctrl.Result{}, err
	}
	if !p.DeletionTimestamp.IsZero() {
		// Children carry owner references (the settings volume only under
		// retainPolicy Delete): garbage collection does the rest, and no
		// finalizer holds the object. The access of other namespaces is
		// controlled by the grants, not by this PgAdmin: it goes now.
		return ctrl.Result{}, r.pruneAccess(ctx, req.NamespacedName, nil)
	}
	before := p.Status.DeepCopy()
	converged, err := r.reconcile(ctx, &p)
	return ctrl.Result{}, r.publish(ctx, &p, before, converged, err)
}

// set is shorthand for a condition write at the object's generation.
func set(p *pgtoolboxv1alpha1.PgAdmin, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, format string, args ...any) {
	conditions.Set(&p.Status.Conditions, p.Generation, conditionType, status, reason, fmt.Sprintf(format, args...))
}

// reconcile runs the concerns in order and reports whether everything
// converged. Progressing and Stalled are decided by publish from that and
// from the error, once per loop, so their transition times only move on a
// real transition.
func (r *Reconciler) reconcile(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (bool, error) {
	conditions.InitializeUnknown(&p.Status.Conditions, p.Generation, append(slices.Clone(aspects),
		pgtoolboxv1alpha1.ConditionProgressing, pgtoolboxv1alpha1.ConditionStalled,
		pgtoolboxv1alpha1.ConditionPgAdminEgressConfined)...)
	if p.IsPaused() {
		// Paused, nothing is applied or observed, with one exception: a
		// hostname outside the allow-list is withdrawn all the same (a
		// pause must not keep a refused name published).
		return false, r.checkHostname(ctx, p)
	}
	in, result, remote, err := r.resolve(ctx, p)
	if err != nil {
		return false, err
	}
	leftovers, err := r.applyChildren(ctx, p, &in)
	if err != nil {
		return false, err
	}
	if err := r.applyAccess(ctx, p, remote, result.Document); err != nil {
		return false, err
	}
	ready, err := r.reconcileWorkload(ctx, p, &in)
	if err != nil {
		return false, err
	}
	r.reconcileExposure(ctx, p, leftovers)
	r.reconcileEgress(p, &in, ready)
	if err := r.reconcileRegistration(ctx, p, &in, result, ready); err != nil {
		return false, err
	}
	// With configDatabase the replica count follows what this loop learnt
	// (the desired image runs, the first registration completed): no
	// later event would bring it.
	if n := desiredReplicas(p, &in); n != in.Replicas {
		in.Replicas = n
		if err := r.applier().Apply(ctx, buildDeployment(p, in)); err != nil {
			return false, r.notOwned(p, err)
		}
	}
	return ready && conditions.IsTrue(p.Status.Conditions, pgtoolboxv1alpha1.ConditionServersSynced) &&
		conditions.IsTrue(p.Status.Conditions, pgtoolboxv1alpha1.ConditionWorkloadReady), nil
}

// resolve reads the inputs and checks what the CRD cannot: the referenced
// Clusters, Poolers and client Secret exist, and every Cluster of another
// namespace is granted (ConfigurationValid).
func (r *Reconciler) resolve(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (buildInputs, offerResult, remoteResolution, error) {
	// The hostname allow-list (EXPOSURE_ALLOWED_HOST_SUFFIXES) before
	// anything else may stall (the image allow-list, the settings
	// database): nothing stays published under a name the installer did
	// not allow, whatever else is wrong.
	if err := r.checkHostname(ctx, p); err != nil {
		return buildInputs{}, offerResult{}, remoteResolution{}, err
	}
	problems := configProblems{reason: pgtoolboxv1alpha1.ReasonConfigurationInvalid}
	selected, remote, err := r.resolveClusters(ctx, p, &problems)
	if err != nil {
		return buildInputs{}, offerResult{}, remoteResolution{}, err
	}
	result, configDB, err := r.offerFor(ctx, p, selected, remote, &problems)
	if err != nil {
		return buildInputs{}, offerResult{}, remoteResolution{}, err
	}
	notes := r.unservedAPIProblems(p, &problems)
	if result.TooMany {
		problems.add(pgtoolboxv1alpha1.ReasonTooManyServers, fmt.Sprintf("%d servers offered, over the limit of %d: narrow the clusters, endpoints or credentials",
			len(result.Document.Servers), offer.MaxServers))
		result.Document.Servers = result.Document.Servers[:offer.MaxServers]
	}
	in, err := r.assembleInputs(p, selected, remote, result, configDB)
	if err != nil {
		return buildInputs{}, offerResult{}, remoteResolution{}, err
	}
	if err := r.checkOAuth2Secret(ctx, p, &in); err != nil {
		return buildInputs{}, offerResult{}, remoteResolution{}, err
	}
	if p.Spec.NetworkPolicy.IsEnabled() {
		in.APIServer = r.apiServerEndpoints(ctx)
	}

	if len(problems.list) > 0 {
		// Not stalled: pgAdmin still serves the Clusters that resolve, and
		// the missing ones are watched.
		set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, problems.reason, "%s", strings.Join(problems.list, "; "))
	} else {
		message := fmt.Sprintf("%d Cluster(s), %d server(s) offered", len(selected)+len(remote.Clusters), len(result.Document.Servers))
		set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			"%s", strings.Join(append([]string{message}, notes...), "; "))
	}
	return in, result, remote, nil
}

// configProblems collects what ConfigurationValid=False names, in the
// order the checks run. Its reason is the one of the last problem that
// carried a reason: the later checks are the more specific.
type configProblems struct {
	reason pgtoolboxv1alpha1.Reason
	list   []string
}

// add records one problem; an empty reason keeps the current one.
func (c *configProblems) add(reason pgtoolboxv1alpha1.Reason, problem ...string) {
	if reason != "" {
		c.reason = reason
	}
	c.list = append(c.list, problem...)
}

// resolveClusters selects the local Clusters and resolves the granted
// remote ones, recording a missing, unselected or ungranted Cluster as a
// problem. Only a failed read is an error.
func (r *Reconciler) resolveClusters(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, problems *configProblems) ([]cnpgv1.Cluster, remoteResolution, error) {
	var clusters cnpgv1.ClusterList
	if err := r.Client.List(ctx, &clusters, client.InNamespace(p.Namespace)); err != nil {
		return nil, remoteResolution{}, fmt.Errorf("list Clusters: %w", err)
	}
	selected, missing, err := selectClusters(p, clusters.Items)
	if err != nil {
		problems.add("", err.Error())
	}
	if len(missing) > 0 {
		problems.add(pgtoolboxv1alpha1.ReasonClusterNotFound, "no Cluster "+strings.Join(missing, ", ")+" in namespace "+p.Namespace)
	}
	if p.Spec.ClusterSelector != nil && len(selected) == 0 && err == nil {
		problems.add(pgtoolboxv1alpha1.ReasonNoClusterSelected, "clusterSelector matches no Cluster in namespace "+p.Namespace)
	}
	remote, err := r.resolveRemote(ctx, p)
	if err != nil {
		return nil, remoteResolution{}, err
	}
	if len(remote.Invalid) > 0 {
		problems.add("", remote.Invalid...)
	}
	if len(remote.Missing) > 0 {
		problems.add(pgtoolboxv1alpha1.ReasonClusterNotFound, "no Cluster "+strings.Join(remote.Missing, ", "))
	}
	if len(remote.Ungranted) > 0 {
		problems.add(pgtoolboxv1alpha1.ReasonGrantMissing, "no PgAdminAccess grants this PgAdmin "+strings.Join(remote.Ungranted, ", ")+
			" (a PgAdminAccess in the Cluster's namespace, naming this PgAdmin and the Cluster)")
	}
	return selected, remote, nil
}

// offerFor computes the offered server set from the Clusters, and the
// DatabaseRoles and Poolers where their APIs are usable, then the
// settings database when one is declared. Its unresolved Poolers and the
// settings database's problem are recorded.
func (r *Reconciler) offerFor(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, selected []cnpgv1.Cluster, remote remoteResolution,
	problems *configProblems,
) (offerResult, configDatabaseState, error) {
	offerIn := offerInputs{PgAdmin: p, Clusters: append(slices.Clone(selected), remote.Clusters...), PrivilegedGranted: remote.PrivilegedGranted}
	// Each namespace's DatabaseRoles and Poolers are read where the
	// operator may list them there (remote ones by remoteClusters); a
	// namespace that refuses one has none of it to offer, and its pooler
	// endpoints say the API is unavailable there.
	here := r.Features.In(p.Namespace)
	offerIn.Roles, offerIn.RolesAPI = remote.Roles, r.Features.DatabaseRoles
	if here.DatabaseRoles.Usable() {
		var roles cnpgv1.DatabaseRoleList
		if err := r.Client.List(ctx, &roles, client.InNamespace(p.Namespace)); err != nil {
			return offerResult{}, configDatabaseState{}, fmt.Errorf("list DatabaseRoles: %w", err)
		}
		offerIn.Roles = slices.Concat(roles.Items, remote.Roles)
	}
	offerIn.Poolers, offerIn.PoolersAPI = remote.Poolers, r.Features.Poolers
	if here.Poolers.Usable() {
		var poolers cnpgv1.PoolerList
		if err := r.Client.List(ctx, &poolers, client.InNamespace(p.Namespace)); err != nil {
			return offerResult{}, configDatabaseState{}, fmt.Errorf("list Poolers: %w", err)
		}
		offerIn.Poolers = slices.Concat(poolers.Items, remote.Poolers)
	}
	result := resolveOffer(offerIn)
	var configDB configDatabaseState
	if p.Spec.ConfigDatabase != nil {
		state, err := r.reconcileConfigDatabase(ctx, p)
		if err != nil {
			return offerResult{}, configDatabaseState{}, err
		}
		configDB = state
		result.Document.ConfigDatabase = state.Server
		if state.Problem != "" {
			problems.add(state.ProblemReason, state.Problem)
		}
	}
	if len(result.Problems) > 0 {
		problems.add(pgtoolboxv1alpha1.ReasonPoolerNotFound, result.Problems...)
	}
	return result, configDB, nil
}

// unservedAPIProblems names an optional API the spec needs and the
// operator cannot read: the probe's answer, rather than a silent gap in
// the offered set. DatabaseRoles are offered where the kind is served
// (every supported CloudNativePG, 1.30 and later). An install whose CRDs
// lack it has none to offer, so that is not a problem, only a note in ConfigurationValid's message: the
// CRD defaults spec.credentials.databaseRoles to true, so a stored true
// cannot tell a request from the default. A refused List is a problem:
// the kind exists and the operator cannot read it.
func (r *Reconciler) unservedAPIProblems(p *pgtoolboxv1alpha1.PgAdmin, problems *configProblems) (notes []string) {
	if p.Spec.Credentials.OffersDatabaseRoles() && !r.Features.In(p.Namespace).DatabaseRoles.Usable() {
		if r.Features.In(p.Namespace).DatabaseRoles.State == discovery.Forbidden {
			problems.add(pgtoolboxv1alpha1.ReasonDatabaseRoleAPIUnavailable, "spec.credentials offers DatabaseRoles, but the DatabaseRole API is unavailable: "+
				unavailableWhy(r.Features.In(p.Namespace).DatabaseRoles))
		} else {
			notes = append(notes, "no DatabaseRole offered: "+unavailableWhy(r.Features.In(p.Namespace).DatabaseRoles))
		}
	}
	if !r.Features.In(p.Namespace).Poolers.Usable() && slices.ContainsFunc(p.Spec.EndpointsOrDefault(), func(e pgtoolboxv1alpha1.Endpoint) bool {
		_, isPooler := e.PoolerName()
		return isPooler
	}) {
		problems.add(pgtoolboxv1alpha1.ReasonPoolerAPIUnavailable, "spec.endpoints names a pooler, but the Pooler API is unavailable: "+unavailableWhy(r.Features.In(p.Namespace).Poolers))
	}
	return notes
}

// assembleInputs builds what the child builders read: the images (checked
// against the installer's allow-list), the Clusters and Poolers pgAdmin
// connects to (NetworkPolicy), and the encoded offer with its hash.
func (r *Reconciler) assembleInputs(p *pgtoolboxv1alpha1.PgAdmin, selected []cnpgv1.Cluster, remote remoteResolution, result offerResult,
	configDB configDatabaseState,
) (buildInputs, error) {
	in := buildInputs{
		ConfigDatabase:        configDB,
		Image:                 specs.ResolveImage(p.Spec.Image, r.Config.PgAdminImage),
		OperatorImage:         r.Config.OperatorImage,
		PlatformAssignsGroups: r.Features.OpenShift,
	}
	if proxy := p.Spec.Auth.OpenShiftOAuthProxy; proxy != nil {
		in.ProxyImage = specs.ResolveImage(proxy.Image, r.Config.OAuthProxyImage)
	}
	// The image allow-list (PGADMIN_IMAGE_REPOSITORIES /
	// --pgadmin-image-repositories), as for the EvidenceAgent: nothing is
	// applied with an image the installer did not allow.
	if err := r.checkImages(p); err != nil {
		return buildInputs{}, err
	}
	for _, cluster := range selected {
		in.Clusters = append(in.Clusters, cluster.Name)
	}
	if c := p.Spec.ConfigDatabase; c != nil && !slices.Contains(in.Clusters, c.ClusterRef.Name) {
		// pgAdmin connects to its settings database too (NetworkPolicy).
		in.Clusters = append(in.Clusters, c.ClusterRef.Name)
	}
	for _, cluster := range remote.Clusters {
		if in.RemoteClusters == nil {
			in.RemoteClusters = map[string][]string{}
		}
		in.RemoteClusters[cluster.Namespace] = append(in.RemoteClusters[cluster.Namespace], cluster.Name)
	}
	for _, endpoint := range p.Spec.EndpointsOrDefault() {
		if name, ok := endpoint.PoolerName(); ok {
			in.Poolers = append(in.Poolers, name)
		}
	}
	encoded, err := offer.Encode(result.Document)
	if err != nil {
		return buildInputs{}, fmt.Errorf("encode the offered set: %w", err)
	}
	in.Offer, in.OfferHash, in.OfferSecrets = encoded, specs.Hash(string(encoded)), result.Document.SecretNames("")
	in.Owners = result.Document.Owners()
	return in, nil
}

// checkOAuth2Secret checks the OAuth2 client Secret exists, by metadata,
// past the cache (Secrets are never cached for content); its version
// rolls the Pod when it carries the reload label. A missing Secret stalls:
// pgAdmin cannot start without it.
func (r *Reconciler) checkOAuth2Secret(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs) error {
	o := p.Spec.Auth.OAuth2
	if o == nil {
		return nil
	}
	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: o.ClientSecretRef.Name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonSecretNotFound,
			"spec.auth.oauth2.clientSecretRef: no Secret %s", o.ClientSecretRef.Name)
		return reconcile.Stall(pgtoolboxv1alpha1.ReasonSecretNotFound,
			"pgAdmin cannot start without its OAuth2 client secret")
	case err != nil:
		return fmt.Errorf("read OAuth2 client Secret metadata: %w", err)
	case secret.Labels[pgtoolboxv1alpha1.LabelReload] == pgtoolboxv1alpha1.ReloadLabelValue:
		in.ClientSecretVersion = secret.ResourceVersion
	}
	return nil
}

// selectClusters returns the Clusters the PgAdmin registers and the names
// of referenced Clusters that do not exist.
func selectClusters(p *pgtoolboxv1alpha1.PgAdmin, all []cnpgv1.Cluster) (selected []cnpgv1.Cluster, missing []string, err error) {
	if p.Spec.ClusterSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(p.Spec.ClusterSelector)
		if err != nil {
			return nil, nil, fmt.Errorf("clusterSelector: %w", err)
		}
		for i := range all {
			if selector.Matches(labels.Set(all[i].Labels)) {
				selected = append(selected, all[i])
			}
		}
		return selected, nil, nil
	}
	byName := map[string]*cnpgv1.Cluster{}
	for i := range all {
		byName[all[i].Name] = &all[i]
	}
	for _, ref := range p.Spec.ClusterRefs {
		if cluster, ok := byName[ref.Name]; ok {
			selected = append(selected, *cluster)
		} else {
			missing = append(missing, ref.Name)
		}
	}
	return selected, missing, nil
}

// apiServerEndpoints reads the default/kubernetes EndpointSlice, the API
// server's addresses after DNAT; on any failure the NetworkPolicy opens the
// usual ports instead, and PgAdminEgressConfined says so.
func (r *Reconciler) apiServerEndpoints(ctx context.Context) apiServerEndpoints {
	var slice discoveryv1.EndpointSlice
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &slice); err != nil {
		logf.FromContext(ctx).V(1).Info("API server endpoints unreadable; the NetworkPolicy opens 443 and 6443", "error", err.Error())
		return nil
	}
	var ports []int32
	for _, port := range slice.Ports {
		if port.Port != nil {
			ports = append(ports, *port.Port)
		}
	}
	var out apiServerEndpoints
	for _, endpoint := range slice.Endpoints {
		for _, address := range endpoint.Addresses {
			out = append(out, apiServerEndpoint{IP: address, Ports: ports})
		}
	}
	slices.SortFunc(out, func(a, b apiServerEndpoint) int { return strings.Compare(a.IP, b.IP) })
	return out
}

// applyChildren writes every child except the Deployment and the Jobs. It
// returns the exposure leftovers applyExposure found.
func (r *Reconciler) applyChildren(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs) ([]exposure.Remnant, error) {
	applier := r.applier()
	n := namesOf(p)
	if p.Spec.ConfigDatabase == nil {
		pvc, err := r.settingsVolume(ctx, p)
		if err != nil {
			return nil, err
		}
		// The settings volume may be adopted by its labels (retainPolicy
		// Retain leaves it without an owner); nothing else ever is.
		if err := applier.ApplyAdopting(ctx, pvc); err != nil {
			return nil, r.notOwned(p, err)
		}
	}
	desired := []client.Object{
		buildServiceAccount(p),
		buildCredentialsRole(p, in.OfferSecrets),
		buildCredentialsRoleBinding(p),
		buildOfferSecret(p, *in),
		buildService(p),
	}
	for _, obj := range desired {
		if err := applier.Apply(ctx, obj); err != nil {
			return nil, r.notOwned(p, err)
		}
	}
	if err := r.ensureGeneratedSecret(ctx, p, n.bootstrap, bootstrapKey, 32); err != nil {
		return nil, r.notOwned(p, err)
	}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		// oauth-proxy wants a 16, 24 or 32-byte cookie secret.
		if err := r.ensureGeneratedSecret(ctx, p, n.proxyCookie, "cookie-secret", 24); err != nil {
			return nil, r.notOwned(p, err)
		}
	} else if err := r.deleteIfOwned(ctx, p, &corev1.Secret{}, n.proxyCookie); err != nil {
		return nil, err
	}

	if policy := buildNetworkPolicy(p, *in); policy != nil {
		if err := applier.Apply(ctx, policy); err != nil {
			return nil, r.notOwned(p, err)
		}
	} else if err := r.deleteIfOwned(ctx, p, &networkingv1.NetworkPolicy{}, n.base); err != nil {
		return nil, err
	}
	return r.applyExposure(ctx, p)
}

// settingsVolume builds the PVC, never shrinking it: a request below the
// current size keeps the current size and is reported.
func (r *Reconciler) settingsVolume(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (*corev1.PersistentVolumeClaim, error) {
	desired := buildSettingsPVC(p)
	var existing corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		current := existing.Spec.Resources.Requests[corev1.ResourceStorage]
		requested := desired.Spec.Resources.Requests[corev1.ResourceStorage]
		if requested.Cmp(current) < 0 {
			desired.Spec.Resources.Requests[corev1.ResourceStorage] = current
			set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigurationInvalid,
				"spec.storage.size %s is below the volume's %s; volumes only grow", requested.String(), current.String())
		}
		// StorageClassName is immutable on the claim: keep what it has.
		desired.Spec.StorageClassName = existing.Spec.StorageClassName
		specs.SetHash(desired, specs.Hash(desired.Spec))
	}
	return desired, nil
}

// ensureGeneratedSecret creates an immutable random Secret once (Get before
// Create, by metadata and past the cache) and never reads it back.
func (r *Reconciler) ensureGeneratedSecret(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, name, key string, size int) error {
	existing := &metav1.PartialObjectMetadata{}
	existing.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, existing)
	if err == nil {
		// Only a Secret this PgAdmin created is used: one planted under the
		// name with our labels (a known cookie secret, say) never is.
		if !reconcile.Controls(existing, generatedSecret(p, name, key, nil), false) {
			return &reconcile.NotOwnedError{Kind: "Secret", Namespace: p.Namespace, Name: name}
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read Secret %s metadata: %w", name, err)
	}
	raw := make([]byte, size)
	if _, err := r.random(raw); err != nil {
		return fmt.Errorf("generate %s: %w", name, err)
	}
	value := make([]byte, base64.RawURLEncoding.EncodedLen(size))
	base64.RawURLEncoding.Encode(value, raw)
	if key == "cookie-secret" {
		value = value[:size]
	}
	if err := r.Client.Create(ctx, generatedSecret(p, name, key, value)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created by someone else between the read and the create.
			return &reconcile.NotOwnedError{Kind: "Secret", Namespace: p.Namespace, Name: name}
		}
		return fmt.Errorf("create Secret %s: %w", name, err)
	}
	return nil
}

// deleteIfOwned deletes a generated object that is no longer wanted.
func (r *Reconciler) deleteIfOwned(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, obj client.Object, name string) error {
	reader := client.Reader(r.Client)
	if _, isSecret := obj.(*corev1.Secret); isSecret {
		// Never a typed read of a Secret: its metadata, past the cache.
		secret := &metav1.PartialObjectMetadata{}
		secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		obj, reader = secret, r.APIReader
	}
	err := reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(obj); owner == nil || owner.UID != p.UID {
		return nil
	}
	if err := r.Client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, name, err)
	}
	return nil
}

// notOwned turns a NotOwnedError into ConfigurationValid=False and Stalled
// (renaming or deleting the foreign object is the user's call).
func (r *Reconciler) notOwned(p *pgtoolboxv1alpha1.PgAdmin, err error) error {
	var notOwned *reconcile.NotOwnedError
	if !errors.As(err, &notOwned) {
		return err
	}
	set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned, "%s", notOwned.Error())
	return reconcile.Stall(pgtoolboxv1alpha1.ReasonResourceNotOwned, "%s", notOwned.Error())
}

// withdrawExposure deletes every exposure object this PgAdmin owns. It
// returns the status sentences of those it could not (leftoverExposure).
func (r *Reconciler) withdrawExposure(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) ([]exposure.Remnant, error) {
	name := namesOf(p).base
	if err := r.deleteIfOwned(ctx, p, &networkingv1.Ingress{}, name); err != nil {
		return nil, err
	}
	if r.Features.In(p.Namespace).Routes.Usable() {
		if err := r.deleteIfOwned(ctx, p, &routev1.Route{}, name); err != nil {
			return nil, err
		}
	}
	if r.Features.In(p.Namespace).Gateways.Usable() {
		if err := r.deleteIfOwned(ctx, p, &gatewayv1.HTTPRoute{}, name); err != nil {
			return nil, err
		}
	}
	return r.leftoverExposure(ctx, p, func(pgtoolboxv1alpha1.ExposureType) bool { return true })
}

// applyExposure writes the exposure object of the requested type and
// deletes the others this PgAdmin owns. It returns, for a kind refused in
// the PgAdmin's namespace, what it published before and may still be
// serving (leftoverExposure).
func (r *Reconciler) applyExposure(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) ([]exposure.Remnant, error) {
	n := namesOf(p)
	kind := p.Spec.Exposure.TypeOrDefault()
	applier := r.applier()
	if kind == pgtoolboxv1alpha1.ExposureTypeIngress {
		if err := applier.Apply(ctx, buildIngress(p)); err != nil {
			return nil, r.notOwned(p, err)
		}
	} else if err := r.deleteIfOwned(ctx, p, &networkingv1.Ingress{}, n.base); err != nil {
		return nil, err
	}
	if r.Features.In(p.Namespace).Routes.Usable() {
		if kind == pgtoolboxv1alpha1.ExposureTypeRoute {
			// Recorded before the write: one that fails may still have
			// published it.
			exposure.MarkPublished(&p.Status.PublishedExposureKinds, "Route")
			if err := applier.Apply(ctx, buildRoute(p)); err != nil {
				return nil, r.notOwned(p, err)
			}
		} else {
			if err := r.deleteIfOwned(ctx, p, &routev1.Route{}, n.base); err != nil {
				return nil, err
			}
			exposure.MarkGone(&p.Status.PublishedExposureKinds, "Route")
		}
	}
	if r.Features.In(p.Namespace).Gateways.Usable() {
		if kind == pgtoolboxv1alpha1.ExposureTypeGateway {
			// Recorded before the write: one that fails may still have
			// published it.
			exposure.MarkPublished(&p.Status.PublishedExposureKinds, "HTTPRoute")
			if err := applier.Apply(ctx, buildHTTPRoute(p)); err != nil {
				return nil, r.notOwned(p, err)
			}
		} else {
			if err := r.deleteIfOwned(ctx, p, &gatewayv1.HTTPRoute{}, n.base); err != nil {
				return nil, err
			}
			exposure.MarkGone(&p.Status.PublishedExposureKinds, "HTTPRoute")
		}
	}
	return r.leftoverExposure(ctx, p, func(t pgtoolboxv1alpha1.ExposureType) bool { return t != kind })
}

// leftoverExposure looks, for each of Route and HTTPRoute refused to the
// operator in the PgAdmin's namespace (while the cluster serves it), for
// the one the PgAdmin may have published before the refusal: the cache
// holds none of that kind there, so one uncached Get each
// (exposure.Leftover), deleting it when remove says so for its type. It
// returns the status sentences of those that remain or may remain.
func (r *Reconciler) leftoverExposure(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, remove func(pgtoolboxv1alpha1.ExposureType) bool) ([]exposure.Remnant, error) {
	here := r.Features.In(p.Namespace)
	var leftovers []exposure.Remnant
	for _, k := range []struct {
		api    discovery.Result
		typ    pgtoolboxv1alpha1.ExposureType
		object client.Object
		kind   string
	}{
		{here.Routes, pgtoolboxv1alpha1.ExposureTypeRoute, &routev1.Route{}, "Route"},
		{here.Gateways, pgtoolboxv1alpha1.ExposureTypeGateway, &gatewayv1.HTTPRoute{}, "HTTPRoute"},
	} {
		if k.api.State == discovery.NotInstalled {
			// Not served: nothing of the kind can remain.
			exposure.MarkGone(&p.Status.PublishedExposureKinds, k.kind)
		}
		if k.api.State != discovery.Forbidden {
			continue
		}
		publishedBefore := exposure.MayHavePublished(p.Spec.Exposure.TypeOrDefault() == k.typ, p.Status.PublishedExposureKinds, k.kind)
		leftover, err := exposure.Leftover(ctx, r.APIReader, r.Client, p, k.object, k.kind, namesOf(p).base, remove(k.typ), publishedBefore)
		if err != nil {
			return nil, err
		}
		if leftover.Gone {
			exposure.MarkGone(&p.Status.PublishedExposureKinds, k.kind)
		}
		if leftover.Sentence != "" {
			leftovers = append(leftovers, leftover)
		}
	}
	return leftovers, nil
}

// reconcileExposure reads the exposure object's status into ExposureReady,
// followed by what the PgAdmin published before a refusal and may still be
// serving (leftovers, from applyExposure): the message never says an
// exposure is gone while one may remain.
func (r *Reconciler) reconcileExposure(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, leftovers []exposure.Remnant) {
	p.Status.URL = exposureURL(p)
	key := client.ObjectKey{Namespace: p.Namespace, Name: namesOf(p).base}
	fact := exposureFact{Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonNotRequested, Message: "in-cluster Service only (exposure type clusterIP)"}
	switch p.Spec.Exposure.TypeOrDefault() {
	case pgtoolboxv1alpha1.ExposureTypeClusterIP:
		// The default fact above.
	case pgtoolboxv1alpha1.ExposureTypeRoute:
		if !r.Features.In(p.Namespace).Routes.Usable() {
			fact = exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonRouteAPIUnavailable, Message: "the Route API is unavailable: " + unavailableWhy(r.Features.In(p.Namespace).Routes)}
			break
		}
		var route routev1.Route
		if err := r.Client.Get(ctx, key, &route); err != nil {
			fact = exposureFact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the Route is not observed yet"}
		} else {
			fact = routeFact(&route)
		}
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		var ingress networkingv1.Ingress
		if err := r.Client.Get(ctx, key, &ingress); err != nil {
			fact = exposureFact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the Ingress is not observed yet"}
		} else {
			fact = ingressFact(&ingress)
		}
	case pgtoolboxv1alpha1.ExposureTypeGateway:
		if !r.Features.In(p.Namespace).Gateways.Usable() {
			fact = exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonGatewayAPIUnavailable, Message: "the Gateway API is unavailable: " + unavailableWhy(r.Features.In(p.Namespace).Gateways)}
			break
		}
		var route gatewayv1.HTTPRoute
		if err := r.Client.Get(ctx, key, &route); err != nil {
			fact = exposureFact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the HTTPRoute is not observed yet"}
		} else {
			fact = httpRouteFact(&route)
		}
	}
	set(p, pgtoolboxv1alpha1.ConditionExposureReady, fact.Status, fact.Reason, "%s", exposure.WithLeftovers(fact.Message, leftovers))
}

// reconcileEgress states, without gating anything, whether the rendered
// NetworkPolicy confines pgAdmin's egress.
func (r *Reconciler) reconcileEgress(p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs, running bool) {
	if !running {
		set(p, pgtoolboxv1alpha1.ConditionPgAdminEgressConfined, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonPgAdminNotRunning,
			"pgAdmin is not running")
		return
	}
	confined, open := egressStatement(buildNetworkPolicy(p, *in))
	if confined {
		set(p, pgtoolboxv1alpha1.ConditionPgAdminEgressConfined, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			"every egress rule names its destinations")
		return
	}
	set(p, pgtoolboxv1alpha1.ConditionPgAdminEgressConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonPgAdminEgressOpen,
		"%s (advisory: whoever opens pgAdmin holds the offered credentials either way)", strings.Join(open, "; "))
}

// reconcileWorkload handles the pre-upgrade backup, applies the Deployment
// and reads WorkloadReady from it and its Pods. It reports whether pgAdmin
// runs the desired template.
func (r *Reconciler) reconcileWorkload(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs) (bool, error) {
	desiredImage := in.Image
	var gate *exposureFact
	in.Replicas = desiredReplicas(p, in)
	if p.Spec.ConfigDatabase != nil && in.ConfigDatabase.Server == nil {
		// Nothing to start against yet: hold the Deployment at zero.
		gate = &exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonConfigDatabaseNotReady, Message: in.ConfigDatabase.Waiting}
	}
	// The pre-upgrade backup copies the SQLite settings database; a
	// configDatabase is backed up by its Cluster.
	if running := lastWriter(p); running != "" && running != desiredImage.Reference && p.Spec.ConfigDatabase == nil {
		done, held, err := r.upgradeBackup(ctx, p, in, running)
		if err != nil {
			return false, err
		}
		if !done {
			gate = &held
			// Keep the old image until its database is backed up; every
			// other change still applies.
			in.Image = specs.ResolvedImage{Reference: running, PullPolicy: corev1.PullIfNotPresent, PullSecrets: desiredImage.PullSecrets}
		}
	}

	desired := buildDeployment(p, *in)
	var existing appsv1.Deployment
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err := r.applier().Apply(ctx, desired); err != nil {
		return false, r.notOwned(p, err)
	}
	// After the write: a refused apply, retried, emits nothing.
	if err == nil && specs.HashOf(&existing) != specs.HashOf(desired) {
		r.event(p, pgtoolboxv1alpha1.ReasonRolloutStarted, "Rollout",
			"pgAdmin Pod template changed (hash %s -> %s)", specs.HashOf(&existing), specs.HashOf(desired))
		if previous := existingImage(&existing); previous != "" && previous != in.Image.Reference && !skippedBackup(p, in.Image.Reference) {
			r.event(p, pgtoolboxv1alpha1.ReasonUpgradeBackupCompleted, "Upgrade",
				"settings database backed up; %s replaces %s", in.Image.Reference, previous)
		}
	}

	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(p.Namespace),
		client.MatchingLabels(identity(p).SelectorLabels(componentPgAdmin))); err != nil {
		return false, fmt.Errorf("list pgAdmin Pods: %w", err)
	}
	fact := workloadFact(desired, &existing, &pods)
	set(p, pgtoolboxv1alpha1.ConditionWorkloadReady, fact.Status, fact.Reason, "%s", fact.Message)
	ready := fact.Status == metav1.ConditionTrue
	if ready {
		p.Status.Image = in.Image.Reference
	}
	if gate != nil {
		// The old image keeps serving (registration may proceed against
		// it), but the desired workload is not reached: say why.
		set(p, pgtoolboxv1alpha1.ConditionWorkloadReady, gate.Status, gate.Reason, "%s", gate.Message)
		if gate.Reason == pgtoolboxv1alpha1.ReasonUpgradeBackupFailed {
			return ready, reconcile.Stall(pgtoolboxv1alpha1.ReasonUpgradeBackupFailed, "%s", gate.Message)
		}
	}
	return ready, nil
}

// upgradeBackup runs the pre-upgrade backup Job for one image change. It
// reports whether the backup completed and, while it has not, the
// WorkloadReady fact that holds the old image.
func (r *Reconciler) upgradeBackup(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs, running string) (bool, exposureFact, error) {
	hash := specs.Hash([]string{running, in.Image.Reference})
	if done := p.Status.UpgradeBackup; done != nil && done.From == running && done.To == in.Image.Reference && done.Hash == hash {
		// Recorded when the Job finished: the Job may be gone (its TTL),
		// and running it again would roll the Deployment back to the old
		// image and prune the real pre-upgrade copy.
		return true, exposureFact{}, nil
	}
	if skip := p.Annotations[pgtoolboxv1alpha1.AnnotationSkipUpgradeBackup]; skip != "" && namesTarget(skip, in.Image.Reference) {
		// The owner's explicit decision for this one target (the old image
		// can no longer run its backup). Never a fallback to the target
		// image: a newer pgAdmin's copy is not a backup of the old
		// database.
		p.Status.UpgradeBackup = &pgtoolboxv1alpha1.PgAdminUpgradeBackupStatus{From: running, To: in.Image.Reference, Hash: hash, Skipped: true}
		r.warn(p, pgtoolboxv1alpha1.ReasonUpgradeBackupSkipped, "Upgrade",
			"pre-upgrade backup skipped by annotation %s: %s replaces %s with no backup of its settings database",
			pgtoolboxv1alpha1.AnnotationSkipUpgradeBackup, in.Image.Reference, running)
		return true, exposureFact{}, nil
	}
	held := exposureFact{
		Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonUpgradeBackupRunning,
		Message: fmt.Sprintf("backing up the settings database before %s replaces %s", in.Image.Reference, running),
	}
	var job batchv1.Job
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: backupJobName(p, hash)}, &job)
	switch {
	case apierrors.IsNotFound(err):
		runningImage := specs.ResolvedImage{Reference: running, PullPolicy: corev1.PullIfNotPresent, PullSecrets: in.Image.PullSecrets}
		if err := r.applier().Apply(ctx, buildBackupJob(p, *in, runningImage, hash, r.Config.UpgradeBackupsKept)); err != nil {
			return false, held, r.notOwned(p, err)
		}
		return false, held, nil
	case err != nil:
		return false, held, err
	}
	finished, failed, message := jobOutcome(&job)
	switch {
	case failed:
		held = exposureFact{
			Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonUpgradeBackupFailed,
			Message: fmt.Sprintf("the pre-upgrade backup with %s failed and it keeps running (delete Job %s to retry; if %s cannot run its backup, e.g. it cannot be pulled or never became Ready, annotate %s=%s to upgrade without one): %s",
				running, job.Name, running, pgtoolboxv1alpha1.AnnotationSkipUpgradeBackup, in.Image.Reference, message),
		}
		return false, held, nil
	case finished:
		// The Event goes with the rollout it releases (once, on the
		// template change), not with every loop that sees the Job done.
		p.Status.UpgradeBackup = &pgtoolboxv1alpha1.PgAdminUpgradeBackupStatus{From: running, To: in.Image.Reference, Hash: hash}
		return true, held, nil
	}
	return false, held, nil
}

// reconcileRegistration runs the registration Job of the current offered
// set once pgAdmin runs, and reads its outcome into ServersSynced.
func (r *Reconciler) reconcileRegistration(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs, result offerResult, running bool) error {
	volumeUID := in.ConfigDatabase.UID
	if p.Spec.ConfigDatabase == nil {
		var pvc corev1.PersistentVolumeClaim
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: namesOf(p).base}, &pvc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		volumeUID = pvc.UID
	}
	hash := registrationHash(p, in.OfferHash, string(volumeUID))
	defer func() { p.Status.Servers = serverRows(result.Rows, p.Status.RegisteredHash == hash) }()

	if err := r.deleteStaleJobs(ctx, p, componentRegistration, registrationJobName(p, hash)); err != nil {
		return err
	}
	if p.Status.RegisteredHash == hash {
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			"%d shared server(s) registered", len(result.Document.Servers))
		return nil
	}
	if !running {
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonPgAdminNotRunning,
			"registration waits for a Ready pgAdmin")
		return nil
	}
	var job batchv1.Job
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: registrationJobName(p, hash)}, &job)
	if apierrors.IsNotFound(err) {
		if err := r.applier().Apply(ctx, buildRegistrationJob(p, *in, hash)); err != nil {
			return r.notOwned(p, err)
		}
		r.event(p, pgtoolboxv1alpha1.ReasonRegistrationStarted, "Register",
			"registering %d shared server(s)", len(result.Document.Servers))
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonRegistrationRunning,
			"registration Job started")
		return nil
	}
	if err != nil {
		return err
	}
	finished, failed, message := jobOutcome(&job)
	switch {
	case failed:
		if detail := r.jobTerminationMessage(ctx, &job); detail != "" {
			message += ": " + detail
		}
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRegistrationFailed,
			"Job %s: %s (delete the Job to retry)", job.Name, message)
	case finished:
		p.Status.RegisteredHash = hash
		p.Status.PersonalServerOwners = slices.Clone(in.Owners)
		if report := parseReport(r.jobTerminationMessage(ctx, &job)); report.PgAdminVersion != "" {
			p.Status.PgAdminVersion = report.PgAdminVersion
		}
		r.event(p, pgtoolboxv1alpha1.ReasonServersRegistered, "Register",
			"%d shared server(s) registered", len(result.Document.Servers))
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			"%d shared server(s) registered", len(result.Document.Servers))
	default:
		set(p, pgtoolboxv1alpha1.ConditionServersSynced, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonRegistrationRunning,
			"registration Job %s is running", job.Name)
	}
	return nil
}

// deleteStaleJobs deletes this PgAdmin's Jobs of component other than keep:
// an outdated registration must not run after the current one.
func (r *Reconciler) deleteStaleJobs(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, component, keep string) error {
	var jobs batchv1.JobList
	if err := r.Client.List(ctx, &jobs, client.InNamespace(p.Namespace), client.MatchingLabels(identity(p).SelectorLabels(component))); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == keep || job.DeletionTimestamp != nil {
			continue
		}
		if owner := metav1.GetControllerOf(job); owner == nil || owner.UID != p.UID {
			continue
		}
		if err := r.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale Job %s: %w", job.Name, err)
		}
	}
	return nil
}

// jobTerminationMessage returns the termination message of the Job's last
// Pod (the registration's report or failure), bounded.
func (r *Reconciler) jobTerminationMessage(ctx context.Context, job *batchv1.Job) string {
	var pods corev1.PodList
	// By the Job's UID, not its name: a Pod anyone could create with a
	// job-name label must not feed the status.
	if err := r.Client.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.ControllerUidLabel: string(job.UID)}); err != nil {
		return ""
	}
	var latest *corev1.Pod
	for i := range pods.Items {
		if latest == nil || latest.CreationTimestamp.Before(&pods.Items[i].CreationTimestamp) {
			latest = &pods.Items[i]
		}
	}
	if latest == nil {
		return ""
	}
	for _, status := range latest.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return reconcile.BoundedMessage(strings.TrimSpace(status.State.Terminated.Message))
		}
	}
	return ""
}

// lastWriter is the image that last ran on the settings database: the
// newest target a backup (or its skip) released, which may have migrated
// the database before ever becoming Ready, else the Ready image. A later
// change is backed up with it, and held on it, never on an older image.
func lastWriter(p *pgtoolboxv1alpha1.PgAdmin) string {
	if b := p.Status.UpgradeBackup; b != nil && b.To != "" && (b.From == p.Status.Image || b.To == p.Status.Image) {
		return b.To
	}
	return p.Status.Image
}

// namesTarget reports whether the skip annotation's value names the target
// image: its full resolved reference, or the digest it is pinned by.
func namesTarget(value, target string) bool {
	return value == target || (strings.HasPrefix(value, "sha256:") && strings.HasSuffix(target, "@"+value))
}

// skippedBackup reports whether the change to target was released by the
// skip annotation rather than by a backup.
func skippedBackup(p *pgtoolboxv1alpha1.PgAdmin, target string) bool {
	b := p.Status.UpgradeBackup
	return b != nil && b.Skipped && b.To == target
}

func (r *Reconciler) warn(p *pgtoolboxv1alpha1.PgAdmin, reason pgtoolboxv1alpha1.Reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(p, nil, corev1.EventTypeWarning, reason.String(), action, "%s", reconcile.BoundedMessage(fmt.Sprintf(note, args...)))
}

func (r *Reconciler) event(p *pgtoolboxv1alpha1.PgAdmin, reason pgtoolboxv1alpha1.Reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(p, nil, corev1.EventTypeNormal, reason.String(), action, "%s", reconcile.BoundedMessage(fmt.Sprintf(note, args...)))
}

func serverRows(rows []pgtoolboxv1alpha1.PgAdminServerStatus, registered bool) []pgtoolboxv1alpha1.PgAdminServerStatus {
	out := slices.Clone(rows)
	for i := range out {
		if registered && out[i].State == pgtoolboxv1alpha1.ServerStatePending {
			out[i].State = pgtoolboxv1alpha1.ServerStateRegistered
		}
	}
	return out
}

// desiredReplicas is the Deployment's replica count this loop (see
// buildInputs.Replicas).
func desiredReplicas(p *pgtoolboxv1alpha1.PgAdmin, in *buildInputs) int32 {
	switch {
	case p.Spec.ConfigDatabase == nil:
		return 1
	case in.ConfigDatabase.Server == nil:
		return 0
	case p.Status.Image != in.Image.Reference || p.Status.RegisteredHash == "":
		// One Pod creates or migrates the settings schema; the others
		// follow once it runs the desired image.
		return 1
	}
	return p.Spec.ReplicasOrDefault()
}

// existingImage is the pgAdmin image a Deployment runs.
func existingImage(d *appsv1.Deployment) string {
	for _, container := range d.Spec.Template.Spec.Containers {
		if container.Name == "pgadmin" {
			return container.Image
		}
	}
	return ""
}

// unavailableWhy is the probe's reason for an unusable optional API.
func unavailableWhy(result discovery.Result) string {
	if result.Reason == "" {
		return "not probed"
	}
	return result.Reason
}

// checkHostname refuses a spec.exposure.hostname outside the operator's
// host suffixes: the exposure objects this PgAdmin owns are withdrawn (one
// published before the allow-list existed must not keep the name),
// ExposureReady=False and status.url cleared, then
// ConfigurationValid=False (HostnameNotAllowed) and a stall until the spec
// or the operator's configuration changes. The message names a refused
// image too; what needs the API (the settings database) is checked once
// the hostname is fixed. It runs while the PgAdmin is paused too. An
// allowed hostname clears a HostnameNotAllowed that conditions still say
// (exposure.ClearHostnameRefusal).
func (r *Reconciler) checkHostname(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) error {
	_, imageProblem := r.imageProblem(p)
	message := exposure.HostnameRefusal(p.Spec.Exposure, r.Config.ExposureAllowedHostSuffixes, imageProblem)
	if message == "" {
		exposure.ClearHostnameRefusal(&p.Status.Conditions, p.Generation)
		return nil
	}
	leftovers, err := r.withdrawExposure(ctx, p)
	if err != nil {
		return err
	}
	p.Status.URL = ""
	set(p, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed, "%s", exposure.WithLeftovers(exposure.WithdrawnMessage, leftovers))
	set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed, "%s", message)
	return reconcile.Stall(pgtoolboxv1alpha1.ReasonHostnameNotAllowed, "%s", message)
}

// checkImages refuses a spec.image or oauth-proxy image outside the
// operator's allow-list: ConfigurationValid=False (ImageNotAllowed) and a
// stall until the spec or the operator's configuration changes.
func (r *Reconciler) checkImages(p *pgtoolboxv1alpha1.PgAdmin) error {
	reason, message := r.imageProblem(p)
	if message == "" {
		return nil
	}
	set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, reason, "%s", message)
	return reconcile.Stall(reason, "%s", message)
}

// imageProblem names the first spec.image or oauth-proxy image outside the
// operator's allow-list, or "" when every image is allowed. It reads
// nothing and writes nothing.
func (r *Reconciler) imageProblem(p *pgtoolboxv1alpha1.PgAdmin) (pgtoolboxv1alpha1.Reason, string) {
	type named struct {
		field        string
		image        *pgtoolboxv1alpha1.ImageSpec
		defaultImage string
	}
	images := []named{{"spec.image", p.Spec.Image, r.Config.PgAdminImage}}
	if proxy := p.Spec.Auth.OpenShiftOAuthProxy; proxy != nil {
		images = append(images, named{"spec.auth.openshiftOAuthProxy.image", proxy.Image, r.Config.OAuthProxyImage})
	}
	for _, candidate := range images {
		if candidate.image == nil || specs.ImageAllowed(candidate.image.Repository, r.Config.PgAdminImageRepositories, candidate.defaultImage) {
			continue
		}
		return pgtoolboxv1alpha1.ReasonImageNotAllowed, fmt.Sprintf("%s repository %s is not in the operator's allow-list (--pgadmin-image-repositories)",
			candidate.field, candidate.image.Repository)
	}
	return "", ""
}
