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

package evidenceagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// certificateState is what the certificate step hands back: when to wake
// up for the next renewal (zero: no timer).
type certificateState struct {
	renewAt time.Time
}

// reconcileCertificate makes the serving certificate exist for the effective
// issuer, sets ServingCertificateReady and status.caBundle.
func (r *Reconciler) reconcileCertificate(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (certificateState, error) {
	if agent.CertificateIssuer() == pgtoolboxv1alpha1.CertificateIssuerOpenShiftServiceCA {
		return certificateState{}, r.reconcileServiceCA(ctx, agent)
	}
	return r.reconcileOperatorCA(ctx, agent)
}

// reconcileOperatorCA keeps the per-agent CA (create-once, immutable,
// replaced only when unusable) and a serving pair it signed for the
// Service names, renewed leafRenewBefore its expiry. Both Secrets are read
// through the APIReader; the CA key never reaches the agent Pod.
func (r *Reconciler) reconcileOperatorCA(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (certificateState, error) {
	now := r.now()
	ca, err := r.readKeyPair(ctx, agent, agent.CASecretName(), caCertKey, caKeyKey)
	if err != nil {
		return certificateState{}, err
	}
	if ca == nil || !caUsable(*ca, now) {
		if ca != nil {
			// Immutable: an unusable CA is replaced by delete + create.
			if err := r.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.CASecretName(), Namespace: agent.Namespace}}); err != nil && !apierrors.IsNotFound(err) {
				return certificateState{}, fmt.Errorf("replace the evidence agent CA: %w", err)
			}
		}
		generated, err := newCA(agent.Namespace+"/"+agent.Name+" evidence agent CA", now)
		if err != nil {
			return certificateState{}, fmt.Errorf("generate the evidence agent CA: %w", err)
		}
		secret := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: objectMeta(agent, agent.CASecretName()),
			Type:       corev1.SecretTypeOpaque,
			Immutable:  new(true),
			Data:       map[string][]byte{caCertKey: generated.Cert, caKeyKey: generated.Key},
		}
		// Get-before-Create for a create-once object.
		if err := r.Client.Create(ctx, secret, client.FieldOwner(reconcile.FieldOwner)); err != nil {
			return certificateState{}, fmt.Errorf("create the evidence agent CA: %w", err)
		}
		ca = &generated
	}

	names := servingDNSNames(agent.WorkloadName(), agent.Namespace)
	leaf, err := r.readKeyPair(ctx, agent, agent.ServingSecretName(), corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	if err != nil {
		return certificateState{}, err
	}
	var renewAt time.Time
	usable := false
	if leaf != nil {
		renewAt, usable = leafRenewal(*leaf, *ca, names, now)
	}
	if !usable {
		issued, err := issueLeaf(*ca, names, now)
		if err != nil {
			return certificateState{}, fmt.Errorf("issue the evidence agent serving certificate: %w", err)
		}
		secret := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: objectMeta(agent, agent.ServingSecretName()),
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: issued.Cert, corev1.TLSPrivateKeyKey: issued.Key, caCertKey: ca.Cert},
		}
		// Mutable on purpose: the agent reloads the mounted pair, so an
		// in-place renewal needs no rollout.
		if err := r.apply(ctx, agent, secret); err != nil {
			return certificateState{}, err
		}
		if renewAt, usable = leafRenewal(issued, *ca, names, now); !usable {
			return certificateState{}, errors.New("the issued serving certificate does not verify")
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(agent, nil, corev1.EventTypeNormal, pgtoolboxv1alpha1.ReasonCertificateIssued.String(), "IssueCertificate",
				"issued the serving certificate for %s, valid until it is renewed at %s", names[2], renewAt.UTC().Format(time.RFC3339))
		}
	}
	agent.Status.CABundle = append([]byte(nil), ca.Cert...)
	setCondition(agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
		"serving certificate signed by the agent's CA for "+names[2])
	return certificateState{renewAt: renewAt}, nil
}

// readKeyPair reads two keys of an operator-owned Secret through the
// APIReader. A missing Secret is nil; a Secret the agent does not control is
// a reconcile.NotOwnedError; missing keys read as an unusable (empty) pair.
func (r *Reconciler) readKeyPair(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, name, certKey, keyKey string) (*keyPair, error) {
	secret := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Secret %s: %w", name, err)
	}
	if owner := metav1.GetControllerOfNoCopy(secret); owner == nil || owner.UID != agent.UID {
		return nil, &reconcile.NotOwnedError{Kind: "Secret", Namespace: agent.Namespace, Name: name}
	}
	return &keyPair{Cert: secret.Data[certKey], Key: secret.Data[keyKey]}, nil
}

// reconcileServiceCA asks the OpenShift service CA for the certificate
// (the Service annotation, applied with the Service) and for its bundle
// (the annotated ConfigMap), and publishes the bundle once injected.
func (r *Reconciler) reconcileServiceCA(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) error {
	if err := r.apply(ctx, agent, buildCABundleConfigMap(agent)); err != nil {
		return err
	}
	configMap := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.CABundleConfigMapName()}, configMap); err != nil {
		return fmt.Errorf("read the service CA bundle ConfigMap: %w", err)
	}
	bundle := []byte(configMap.Data[serviceCABundleKey])
	served := &corev1.Secret{}
	servedErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.ServiceCASecretName()}, served)
	if servedErr != nil && !apierrors.IsNotFound(servedErr) {
		return fmt.Errorf("check the service CA serving Secret: %w", servedErr)
	}
	names := servingDNSNames(agent.WorkloadName(), agent.Namespace)
	switch {
	case !validPEMBundle(bundle):
		setCondition(agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending,
			fmt.Sprintf("ConfigMap %s has no injected %s yet (is the OpenShift service CA operator running?)", configMap.Name, serviceCABundleKey))
	case servedErr != nil:
		setCondition(agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending,
			fmt.Sprintf("the service CA has not created Secret %s yet", agent.ServiceCASecretName()))
	case !chainsTo(served.Data[corev1.TLSCertKey], bundle, names[2], r.now()):
		// The ConfigMap is writable by the namespace's editors: publish a
		// bundle only when it verifies the certificate the agent serves.
		setCondition(agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending,
			fmt.Sprintf("the bundle in ConfigMap %s does not verify the certificate in Secret %s for %s", configMap.Name, served.Name, names[2]))
	default:
		agent.Status.CABundle = bundle
		setCondition(agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			"serving certificate issued by the OpenShift service CA")
	}
	return nil
}
