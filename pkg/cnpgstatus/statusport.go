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

package cnpgstatus

import (
	"slices"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// postgresContainerName is specs.PostgresContainerName (pkg/specs/pods.go
// v1.30.0:78).
const postgresContainerName = "postgres"

// statusPortTLSFlag is the instance-manager flag the operator appends
// to the postgres container Command when the status port serves HTTPS
// (pkg/specs/pods.go v1.30.0:297, every instance created by 1.24+).
const statusPortTLSFlag = "--status-port-tls"

// StatusPortTLS is CloudNativePG's GetStatusSchemeFromPod at v1.30.0
// (pkg/management/postgres/webserver/client/remote/instance.go:380-396):
// the first container named "postgres"
// decides, and it serves HTTPS exactly when its Command contains
// --status-port-tls. False means the Pod was created before CNPG 1.24
// and never recreated: the console shows an error row ("status port
// without TLS: restart the instance") and never reads it over HTTP, which
// would send the instance's status in clear. A nil Pod is false.
//
// On 1.31 (planned) the flag is gone and the port is always HTTPS; use
// StatusPortTLSForMinor when the minor is known.
func StatusPortTLS(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != postgresContainerName {
			continue
		}
		return slices.Contains(container.Command, statusPortTLSFlag)
	}
	return false
}

// StatusPortTLSForMinor is StatusPortTLS with the planned 1.31 rule: an
// instance manager of 1.31 always serves HTTPS and no longer receives
// the flag (upstream main 6595c4a86 "remove status-port-tls flag and
// HTTP fallback": internal/cmd/manager/instance/run/cmd.go main:151-158
// accepts and ignores it, pkg/specs/podspec_diff.go main:185-198 drops
// it from comparisons, GetStatusSchemeFromPod no longer exists). minor
// must be the minor of the instance manager running in that Pod when it
// is known; for 1.30 the Pod Command decides. A nil Pod, or a
// Pod without a postgres container, is false.
func StatusPortTLSForMinor(minor Minor, pod *corev1.Pod) bool {
	if minor != Minor131 {
		return StatusPortTLS(pod)
	}
	if pod == nil {
		return false
	}
	return slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == postgresContainerName
	})
}

// StatusServerName is the name the console verifies the status port's
// server certificate against: the first enabled default Service in the
// order GetClusterAltDNSNames lists them (api/v1/cluster_funcs.go
// v1.30.0:1142-1167: rw, r, ro, each as name, name.ns, name.ns.svc,
// name.ns.svc.<domain>), as "<cluster>-<suffix>.<namespace>.svc". The
// operator-generated server certificate carries those names; the status
// port serves that same certificate (pkg/management/postgres/webserver/remote.go
// v1.30.0:192-201). "" when the Cluster is nil, unnamed, or has every
// default Service disabled.
//
// Not knowable from here: with a user-provided spec.certificates.serverTLSSecret
// the certificate may carry none of these names. CloudNativePG itself
// verifies the status port against the CA only and ignores the DNS name
// for exactly that reason (pkg/certs/tls.go v1.30.0:65-67).
func StatusServerName(c *cnpgv1.Cluster) string {
	if c == nil || c.Name == "" || c.Namespace == "" {
		return ""
	}
	var disabled []cnpgv1.ServiceSelectorType
	if c.Spec.Managed != nil && c.Spec.Managed.Services != nil {
		disabled = c.Spec.Managed.Services.DisabledDefaultServices
	}
	// cluster_funcs.go v1.30.0:1456-1481 (Is{ReadWrite,Read,ReadOnly}ServiceEnabled)
	// and the suffixes of api/v1/cluster_types.go v1.30.0:65-74.
	for _, service := range []struct {
		selector cnpgv1.ServiceSelectorType
		suffix   string
	}{
		{cnpgv1.ServiceSelectorTypeRW, cnpgv1.ServiceReadWriteSuffix},
		{cnpgv1.ServiceSelectorTypeR, cnpgv1.ServiceReadSuffix},
		{cnpgv1.ServiceSelectorTypeRO, cnpgv1.ServiceReadOnlySuffix},
	} {
		if !slices.Contains(disabled, service.selector) {
			return c.Name + service.suffix + "." + c.Namespace + ".svc"
		}
	}
	return ""
}
