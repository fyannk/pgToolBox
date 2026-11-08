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

package v1alpha1

import (
	"time"
)

// CatalogueEvidenceAgent is the owner name of the EvidenceAgent reasons in
// Catalogue.
const CatalogueEvidenceAgent = "evidenceagent"

// evidenceAgentReasons are the EvidenceAgent-specific reasons of the frozen
// catalogue.
var evidenceAgentReasons = []Reason{
	ReasonBarmanPluginNotConfigured,
	ReasonObjectStoreNotFound,
	ReasonObjectStoreAPIUnavailable,
	ReasonUnsupportedObjectStore,
	ReasonDestinationInvalid,
	ReasonRegionRequired,
	ReasonImageRequired,
	ReasonServiceCAPending,
	ReasonCertificateIssued,
}

func init() {
	SchemeBuilder.Register(&EvidenceAgent{}, &EvidenceAgentList{})
	registerReasons(CatalogueEvidenceAgent, evidenceAgentReasons...)
}

// LabelEvidenceAgent carries the owning EvidenceAgent's name on every object
// the operator generates for it.
const LabelEvidenceAgent = "pgtoolbox.fyannk.dev/evidenceagent"

// DefaultEvidenceAgentRefreshInterval is the repository re-read interval
// when spec.refreshInterval is omitted.
const DefaultEvidenceAgentRefreshInterval = 5 * time.Minute

// WorkloadName is the name of the agent's Deployment, Service and
// ServiceAccount: "<name>-evidence-agent" (at most 55 characters, since the
// CR name is at most 40).
func (a *EvidenceAgent) WorkloadName() string { return a.Name + "-evidence-agent" }

// ServingSecretName is the Secret holding the serving key pair the agent
// mounts.
func (a *EvidenceAgent) ServingSecretName() string { return a.Name + "-evidence-agent-tls" }

// ServiceCASecretName is the Secret the OpenShift service CA writes the
// serving key pair into (a name distinct from ServingSecretName, so the two
// issuers never contend for one Secret).
func (a *EvidenceAgent) ServiceCASecretName() string { return a.Name + "-evidence-agent-service-tls" }

// EffectiveServingSecretName is the Secret the agent mounts for the
// effective issuer.
func (a *EvidenceAgent) EffectiveServingSecretName() string {
	if a.CertificateIssuer() == CertificateIssuerOpenShiftServiceCA {
		return a.ServiceCASecretName()
	}
	return a.ServingSecretName()
}

// CASecretName is the Secret holding the operator-issued CA key pair; the
// agent Pod never mounts it. Unused with the OpenShift service CA.
func (a *EvidenceAgent) CASecretName() string { return a.Name + "-evidence-agent-ca" }

// CABundleConfigMapName is the ConfigMap the OpenShift service CA injects
// its bundle into. Unused with the operator issuer.
func (a *EvidenceAgent) CABundleConfigMapName() string { return a.Name + "-evidence-agent-ca" }

// CertificateIssuer returns the effective issuer.
func (a *EvidenceAgent) CertificateIssuer() EvidenceAgentCertificateIssuer {
	if a.Spec.TLS == nil || a.Spec.TLS.Issuer == "" {
		return CertificateIssuerOperator
	}
	return a.Spec.TLS.Issuer
}

// RefreshInterval returns the effective repository re-read interval.
func (a *EvidenceAgent) RefreshInterval() time.Duration {
	if a.Spec.RefreshInterval == nil || a.Spec.RefreshInterval.Duration <= 0 {
		return DefaultEvidenceAgentRefreshInterval
	}
	return a.Spec.RefreshInterval.Duration
}
