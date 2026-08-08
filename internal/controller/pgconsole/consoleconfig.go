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
	"fmt"
	"strings"
	"time"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The console reads its whole configuration from the environment and
// refuses to serve on a value it rejects, so this file is the seam where a
// spec becomes something the application will accept. Two rules keep that
// honest:
//
//   - Capabilities are always rendered. They are policy, they decide which
//     rules the generated Roles carry, and a reader of the Pod should not
//     have to know the application's defaults to know what is switched on.
//   - Tunables are rendered only when the spec sets them. The operator does
//     not restate the application's numeric defaults, so the application
//     stays the single owner of what "default" means and the two cannot
//     drift apart silently.
//
// validateConsoleSpec below enforces the same bounds the application
// enforces, so a rendered value is never one the console will reject.

// consoleTrustedUserHeader is the identity header the proxy sets. It is
// display and audit attribution only, never authorization.
const consoleTrustedUserHeader = "X-Forwarded-User"

// consoleTrustedLevelHeader is the authorization-level header the proxy
// sets and the console reads. It is rendered explicitly rather than left to
// the application's default because it is the contract between the two
// binaries: pgtoolbox-proxy strips any inbound copy and sets its own, and
// the NetworkPolicy confining ingress to the proxy is what makes it
// trustworthy.
const consoleTrustedLevelHeader = "X-PgToolBox-Level"

// pgAdminLinkPath is the proxy route prefix that reaches the embedded
// pgAdmin, and so the suffix of the console's pgAdmin link-out.
const pgAdminLinkPath = "/pgadmin"

// Bounds the console applies to its own configuration. Duplicated here so
// admission and the controller reject a value before it reaches a Pod that
// would refuse to start on it; the integer bounds live on the CRD fields as
// kubebuilder minimum/maximum markers instead.
const (
	minEventsMaxAge = time.Minute
	maxEventsMaxAge = 24 * time.Hour

	minAPIRequestTimeout = time.Second
	maxAPIRequestTimeout = time.Minute

	minHistoryCoalesceWindow = time.Second
	maxHistoryCoalesceWindow = time.Hour

	minMetricsInterval = 5 * time.Second
	maxMetricsInterval = 5 * time.Minute

	minMetricsRetention = time.Hour
	maxMetricsRetention = 30 * 24 * time.Hour
)

// consoleAllowOperations reports whether the day-2 operation surface is
// served. It also decides whether the operate Role carries the mutation
// rules, so the flag and the authority move together.
func consoleAllowOperations(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.AllowOperations == nil || *console.Spec.Console.AllowOperations
}

// consoleAllowLogs reports whether the bounded log tail is served, and so
// whether the read Role carries the pods/log rule.
func consoleAllowLogs(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.AllowLogs == nil || *console.Spec.Console.AllowLogs
}

// consoleAllowAccessReview reports whether the dba review panel is served,
// and so whether the operate Role carries the access-request rules.
func consoleAllowAccessReview(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.AllowAccessReview == nil || *console.Spec.Console.AllowAccessReview
}

// consoleAllowClusterCatalogs reports whether the console may read the one
// cluster-scoped ClusterImageCatalog its Cluster references. Opt-in: it is
// the only authority a console holds outside its namespace.
func consoleAllowClusterCatalogs(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.AllowClusterCatalogs != nil && *console.Spec.Console.AllowClusterCatalogs
}

// consoleAllowInsecureLinks reports whether http link-out URLs are allowed.
func consoleAllowInsecureLinks(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.AllowInsecureLinks != nil && *console.Spec.Console.AllowInsecureLinks
}

// consoleHistoryEnabled reports whether object definition history is kept.
func consoleHistoryEnabled(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.History.Enabled == nil || *console.Spec.Console.History.Enabled
}

// consoleMetricsEnabled reports whether the metrics window is collected.
func consoleMetricsEnabled(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.Console.Metrics.Enabled == nil || *console.Spec.Console.Metrics.Enabled
}

// consolePgAdminURL is the console's pgAdmin link-out: the console's own
// external base URL plus the proxy route prefix that reaches the embedded
// pgAdmin, so the link traverses the same authentication boundary as every
// other request and is subject to spec.pgAdmin.accessMinLevel.
//
// The application requires an absolute https URL unless insecure links are
// allowed, so a console with no exposure hostname renders no link rather
// than an http one the application would refuse to start on. The link is
// then simply absent, which is what a port-forward user sees today.
func consolePgAdminURL(console *pgtoolboxv1alpha1.PgConsole) string {
	if !pgAdminEnabled(console) {
		return ""
	}
	base := consoleBaseURL(console)
	if strings.HasPrefix(base, "http://") && !consoleAllowInsecureLinks(console) {
		return ""
	}
	return base + pgAdminLinkPath
}

// consoleEnv renders the pgconsole container's environment in a fixed
// order: identity, then capabilities, then the tunables the spec sets, then
// the link-outs. The order is part of the deterministic rendering contract —
// a no-op reconcile must produce byte-identical containers — so entries are
// appended, never keyed off a map.
func consoleEnv(console *pgtoolboxv1alpha1.PgConsole) []corev1.EnvVar {
	spec := console.Spec.Console

	env := []corev1.EnvVar{
		{Name: "CLUSTER_NAME", Value: console.Spec.CNPGClusterRef.Name},
		{Name: "NAMESPACE", Value: console.Namespace},
		// Identity and level arrive from the proxy as trusted headers; they
		// are only trustworthy because the NetworkPolicy confines ingress to
		// the proxy.
		{Name: "TRUSTED_USER_HEADER", Value: consoleTrustedUserHeader},
		{Name: "TRUSTED_LEVEL_HEADER", Value: consoleTrustedLevelHeader},
		// Capabilities are always stated, never inherited: each one also
		// decides a rule in the generated Roles.
		{Name: "ALLOW_OPERATIONS", Value: boolString(consoleAllowOperations(console))},
		{Name: "ALLOW_LOGS", Value: boolString(consoleAllowLogs(console))},
		{Name: "ALLOW_ACCESS_REVIEW", Value: boolString(consoleAllowAccessReview(console))},
		{Name: "ALLOW_CLUSTER_CATALOGS", Value: boolString(consoleAllowClusterCatalogs(console))},
		{Name: "ALLOW_INSECURE_LINKS", Value: boolString(consoleAllowInsecureLinks(console))},
	}

	// Tunables: rendered only when set, so the application keeps ownership
	// of every numeric default.
	if spec.LogTail.Lines != nil {
		env = append(env, corev1.EnvVar{Name: "LOG_TAIL_LINES", Value: int32String(*spec.LogTail.Lines)})
	}
	if spec.LogTail.MaxBytes != nil {
		env = append(env, corev1.EnvVar{Name: "LOG_TAIL_MAX_BYTES", Value: int32String(*spec.LogTail.MaxBytes)})
	}
	if spec.EventsMaxAge != nil {
		env = append(env, corev1.EnvVar{Name: "EVENTS_MAX_AGE", Value: spec.EventsMaxAge.Duration.String()})
	}
	if spec.APIRequestTimeout != nil {
		env = append(env, corev1.EnvVar{Name: "API_REQUEST_TIMEOUT", Value: spec.APIRequestTimeout.Duration.String()})
	}

	env = append(env, corev1.EnvVar{Name: "HISTORY_ENABLED", Value: boolString(consoleHistoryEnabled(console))})
	if spec.History.MaxRevisions != nil {
		env = append(env, corev1.EnvVar{Name: "HISTORY_MAX_REVISIONS", Value: int32String(*spec.History.MaxRevisions)})
	}
	if spec.History.MaxBytes != nil {
		env = append(env, corev1.EnvVar{Name: "HISTORY_MAX_BYTES", Value: int32String(*spec.History.MaxBytes)})
	}
	if spec.History.PerObjectRevisions != nil {
		env = append(env, corev1.EnvVar{
			Name:  "HISTORY_PER_OBJECT_REVISIONS",
			Value: int32String(*spec.History.PerObjectRevisions),
		})
	}
	if spec.History.CoalesceWindow != nil {
		env = append(env, corev1.EnvVar{
			Name:  "HISTORY_COALESCE_WINDOW",
			Value: spec.History.CoalesceWindow.Duration.String(),
		})
	}

	env = append(env, corev1.EnvVar{Name: "METRICS_ENABLED", Value: boolString(consoleMetricsEnabled(console))})
	if spec.Metrics.Interval != nil {
		env = append(env, corev1.EnvVar{Name: "METRICS_INTERVAL", Value: spec.Metrics.Interval.Duration.String()})
	}
	if spec.Metrics.Retention != nil {
		env = append(env, corev1.EnvVar{Name: "METRICS_RETENTION", Value: spec.Metrics.Retention.Duration.String()})
	}

	if url := consolePgAdminURL(console); url != "" {
		env = append(env, corev1.EnvVar{Name: "PGADMIN_URL", Value: url})
	}
	if spec.MonitoringURL != "" {
		env = append(env, corev1.EnvVar{Name: "MONITORING_URL", Value: spec.MonitoringURL})
	}
	return env
}

// validateConsoleSpec rejects a console configuration the application would
// reject at startup. The CEL rules and the integer bounds on the CRD catch
// most of it at admission; this re-check covers objects that predate them,
// and the durations, whose bounds are not expressible as a plain pattern.
func validateConsoleSpec(console *pgtoolboxv1alpha1.PgConsole) error {
	spec := console.Spec.Console

	durations := []struct {
		field    string
		value    *metav1.Duration
		min, max time.Duration
	}{
		{"spec.console.eventsMaxAge", spec.EventsMaxAge, minEventsMaxAge, maxEventsMaxAge},
		{"spec.console.apiRequestTimeout", spec.APIRequestTimeout, minAPIRequestTimeout, maxAPIRequestTimeout},
		{
			"spec.console.history.coalesceWindow",
			spec.History.CoalesceWindow,
			minHistoryCoalesceWindow,
			maxHistoryCoalesceWindow,
		},
		{"spec.console.metrics.interval", spec.Metrics.Interval, minMetricsInterval, maxMetricsInterval},
		{"spec.console.metrics.retention", spec.Metrics.Retention, minMetricsRetention, maxMetricsRetention},
	}
	for _, d := range durations {
		if d.value == nil {
			continue
		}
		if d.value.Duration < d.min || d.value.Duration > d.max {
			return fmt.Errorf("%s must be between %s and %s", d.field, d.min, d.max)
		}
	}

	if strings.HasPrefix(spec.MonitoringURL, "http://") && !consoleAllowInsecureLinks(console) {
		return fmt.Errorf("spec.console.monitoringURL must use https unless spec.console.allowInsecureLinks is true")
	}
	return nil
}

// boolString renders a boolean the way the console parses it: the strings
// "true" and "false", and nothing else.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// int32String renders an integer configuration value.
func int32String(value int32) string { return fmt.Sprintf("%d", value) }
