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
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CataloguePgToolBoxConsole is the owner name of the PgToolBoxConsole reasons in
// Catalogue.
const CataloguePgToolBoxConsole = "pgtoolboxconsole"

// pgToolBoxConsoleReasons are the PgToolBoxConsole-specific reasons of the frozen
// catalogue.
var pgToolBoxConsoleReasons = []Reason{
	ReasonConsoleImageRequired,
	ReasonConsoleRoleMissing,
	ReasonInstanceCAMissing,
	ReasonInstanceCAInvalid,
	ReasonInstanceCABundleFull,
	ReasonExposureOpen,
	ReasonExposurePeersBroad,
	ReasonExposurePolicyInvalid,
}

func init() {
	SchemeBuilder.Register(&PgToolBoxConsole{}, &PgToolBoxConsoleList{})
	registerReasons(CataloguePgToolBoxConsole, pgToolBoxConsoleReasons...)
}

// LabelPgToolBoxConsole carries the owning PgToolBoxConsole's name on every object the
// operator generates for it.
const LabelPgToolBoxConsole = "pgtoolbox.fyannk.dev/console"

// AnnotationUserManaged=UserManagedValue on a ConfigMap the operator (or the
// chart) seeded hands it over to people: neither writes it again.
const (
	AnnotationUserManaged = "pgtoolbox.fyannk.dev/user-managed"
	UserManagedValue      = "true"
)

// WorkloadName is the name of the console's Deployment, Service,
// ServiceAccount and exposure: the PgToolBoxConsole's name when it already says
// "console", else "<name>-console" — the chart's rule for a release name.
func (c *PgToolBoxConsole) WorkloadName() string {
	if strings.Contains(c.Name, "console") {
		return c.Name
	}
	return c.Name + "-console"
}

// SessionSecretName is the Secret holding the cookie-sealing key.
func (c *PgToolBoxConsole) SessionSecretName() string { return c.WorkloadName() + "-session" }

// RevocationsConfigMapName is the revocation list every replica watches.
func (c *PgToolBoxConsole) RevocationsConfigMapName() string {
	return c.WorkloadName() + "-revocations"
}

// StateConfigMapName is the state the console's replicas share (sign-outs,
// "since" times); the console writes it, the operator only creates it.
func (c *PgToolBoxConsole) StateConfigMapName() string { return c.WorkloadName() + "-state" }

// LifecycleConfigMapName is the ConfigMap the console reads its end-of-life
// table from: the referenced one, else the one the operator seeds.
func (c *PgToolBoxConsole) LifecycleConfigMapName() string {
	if c.Spec.LifecycleConfigMapRef != nil {
		return c.Spec.LifecycleConfigMapRef.Name
	}
	return c.WorkloadName() + "-lifecycle"
}

// HistoryPersistent reports whether the history is kept on volumes: the
// API server defaults spec.history.persistent to true, and an object built
// without it (a test, a client that bypasses defaulting) reads the same.
func (c *PgToolBoxConsole) HistoryPersistent() bool {
	return c.Spec.History.Persistent == nil || *c.Spec.History.Persistent
}

// HistoryClaimTemplateName names the StatefulSet's claim template; each
// replica's claim is history-<workload>-<ordinal>.
const HistoryClaimTemplateName = "history"

// DigestLeaseName is the Lease whose holder delivers the daily digests.
func (c *PgToolBoxConsole) DigestLeaseName() string { return c.WorkloadName() + "-digest" }

// MCPLeaseName is the Lease whose holder alone serves MCP, and the name of
// the selector-less Service the exposure sends the MCP paths to.
func (c *PgToolBoxConsole) MCPLeaseName() string { return c.WorkloadName() + "-mcp" }

// DigestDelivered reports whether a digest delivery channel is configured.
func (c *PgToolBoxConsole) DigestDelivered() bool { return c.Spec.Digest.Delivery != nil }

// InstanceCAMirrored reports whether the operator keeps the instance CA
// bundle: spec.instanceCABundleRef is omitted.
func (c *PgToolBoxConsole) InstanceCAMirrored() bool { return c.Spec.InstanceCABundleRef == nil }

// InstanceCAMirrorName is the ConfigMap the operator mirrors the selected
// Clusters' server CAs into.
func (c *PgToolBoxConsole) InstanceCAMirrorName() string { return c.Name + "-instance-ca" }

// InstanceCABundleName is the instance CA bundle ConfigMap the console
// mounts: the referenced one, else the operator's mirror.
func (c *PgToolBoxConsole) InstanceCABundleName() string {
	if c.Spec.InstanceCABundleRef != nil {
		return c.Spec.InstanceCABundleRef.Name
	}
	return c.InstanceCAMirrorName()
}

// AuthProvider returns the effective sign-in provider.
func (c *PgToolBoxConsole) AuthProvider() PgToolBoxConsoleAuthProvider {
	if c.Spec.Auth.Provider == "" {
		return PgToolBoxConsoleAuthToken
	}
	return c.Spec.Auth.Provider
}

// EffectiveReplicas is the replica count, with or without persistent
// history (each replica has its own volume).
func (c *PgToolBoxConsole) EffectiveReplicas() int32 {
	if c.Spec.Replicas == nil {
		return 2
	}
	return *c.Spec.Replicas
}

// EffectivePublicURL is spec.publicURL, else https://<exposure.hostname>,
// without a trailing slash; "" when neither is set.
func (c *PgToolBoxConsole) EffectivePublicURL() string {
	if c.Spec.PublicURL != "" {
		return strings.TrimSuffix(c.Spec.PublicURL, "/")
	}
	if c.Spec.Exposure.Hostname != "" {
		return "https://" + c.Spec.Exposure.Hostname
	}
	return ""
}

// PodLogsAllowed reports whether the console may read Pod logs.
func (c *PgToolBoxConsole) PodLogsAllowed() bool { return c.Spec.PodLogs == nil || *c.Spec.PodLogs }

// RestoreDrillsEnabled reports whether the console offers restore drills.
func (c *PgToolBoxConsole) RestoreDrillsEnabled() bool {
	return c.Spec.RestoreDrills != nil && c.Spec.RestoreDrills.Enabled && c.OperationsEnabled()
}

// RestoreDrillNetworkPolicyEnabled reports whether the operator renders
// the drill Pods' NetworkPolicy: with drills, unless turned off.
func (c *PgToolBoxConsole) RestoreDrillNetworkPolicyEnabled() bool {
	if !c.RestoreDrillsEnabled() {
		return false
	}
	enabled := c.Spec.RestoreDrills.NetworkPolicy.Enabled
	return enabled == nil || *enabled
}

// OperationsEnabled reports whether the operations endpoints are served.
func (c *PgToolBoxConsole) OperationsEnabled() bool {
	return c.Spec.Operations == nil || *c.Spec.Operations
}

// MCPEnabled reports whether the console serves the MCP endpoint.
func (c *PgToolBoxConsole) MCPEnabled() bool {
	return c.Spec.MCP != nil && c.Spec.MCP.Enabled
}

// MCPWriteTools reports whether the MCP write tools are asked
// for; CEL refuses them without the endpoint.
func (c *PgToolBoxConsole) MCPWriteTools() bool {
	return c.MCPEnabled() && c.Spec.MCP.WriteTools
}

// MCPClientMetadataDocuments reports whether the console accepts OAuth
// client ID metadata documents, and so fetches from the Internet: only
// with the endpoint on.
func (c *PgToolBoxConsole) MCPClientMetadataDocuments() bool {
	return c.MCPEnabled() && c.Spec.MCP.OAuthClientMetadataDocuments
}

// TracingEnabled reports whether the console exports traces.
func (c *PgToolBoxConsole) TracingEnabled() bool {
	return c.Spec.Tracing != nil && c.Spec.Tracing.Endpoint != ""
}

// EvidenceEnabled reports whether the console reads repository evidence.
func (c *PgToolBoxConsole) EvidenceEnabled() bool {
	return c.Spec.Evidence.Enabled == nil || *c.Spec.Evidence.Enabled
}

// SessionIdleTimeout is the effective idle timeout.
func (c *PgToolBoxConsole) SessionIdleTimeout() time.Duration {
	if t := c.Spec.Auth.SessionIdleTimeout; t != nil && t.Duration > 0 {
		return t.Duration
	}
	return time.Hour
}

// NetworkPolicyPeer is the peer as a NetworkPolicy writes it, deep-copied.
func (p *PgToolBoxConsolePolicyPeer) NetworkPolicyPeer() networkingv1.NetworkPolicyPeer {
	peer := networkingv1.NetworkPolicyPeer{
		PodSelector:       p.PodSelector.LabelSelector(),
		NamespaceSelector: p.NamespaceSelector.LabelSelector(),
	}
	if p.IPBlock != nil {
		peer.IPBlock = &networkingv1.IPBlock{CIDR: p.IPBlock.CIDR, Except: append([]string(nil), p.IPBlock.Except...)}
	}
	return peer
}

// LabelSelector is the selector as the API writes it, nil for nil.
func (s *PgToolBoxConsoleLabelSelector) LabelSelector() *metav1.LabelSelector {
	if s == nil {
		return nil
	}
	selector := &metav1.LabelSelector{}
	if s.MatchLabels != nil {
		selector.MatchLabels = make(map[string]string, len(s.MatchLabels))
		for k, v := range s.MatchLabels {
			selector.MatchLabels[k] = string(v)
		}
	}
	for _, e := range s.MatchExpressions {
		requirement := metav1.LabelSelectorRequirement{Key: e.Key, Operator: e.Operator}
		for _, v := range e.Values {
			requirement.Values = append(requirement.Values, string(v))
		}
		selector.MatchExpressions = append(selector.MatchExpressions, requirement)
	}
	return selector
}
