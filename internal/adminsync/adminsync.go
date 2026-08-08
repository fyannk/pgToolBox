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

// Package adminsync applies PgToolBoxUser provisioning to pgAdmin through
// an in-pod HTTPS API backed by pgAdmin's supported setup.py CLI. It is the
// only package allowed to talk to the in-pod admin-sync API.
package adminsync

import (
	"context"
	"fmt"
	"sort"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	pythonPath = "/venv/bin/python3"
	setupPath  = "/pgadmin4/setup.py"
)

// Admin-sync modes selected by the caller.
const (
	// ModeSidecar calls the in-pod admin-sync API served by the operator
	// binary injected into the pgAdmin Pod.
	ModeSidecar = "sidecar"
	// ModeDisabled skips pgAdmin synchronization entirely.
	ModeDisabled = "disabled"
)

// SidecarPort is the fixed in-pod port of the admin-sync API.
const SidecarPort = 9600

// SidecarServerName is the DNS SAN of the generated serving certificate;
// the operator dials Pod IPs and verifies against this name and the
// pinned CA.
const SidecarServerName = "pgadmin-admin-sync"

// DefaultPassFilePath is the mounted path where the sidecar writes the
// combined pgpass file and where the server JSON points back to.
// #nosec G101 -- filesystem path; no credential material.
const DefaultPassFilePath = "/run/pgadmin/passfile/pgpass"

// DefaultSettingsDBPath is pgAdmin's settings database on the settings
// volume. The sidecar reads it to discover which accounts exist; it never
// writes it, leaving every mutation to pgAdmin's own setup.py.
const DefaultSettingsDBPath = "/var/lib/pgadmin/pgadmin4.db"

// Request carries the complete desired pgAdmin state for one PgConsole.
type Request struct {
	Namespace   string
	ConsoleName string
	Selector    map[string]string
	Checksum    string
	Servers     []Server
}

// Server is one connection pgAdmin should offer, built from a credential
// the CloudNativePG cluster itself publishes. Every pgAdmin account gets
// the same list: the credentials belong to the cluster, not to whoever
// signed in, and everyone who reaches pgAdmin has already been admitted at
// the dba level the proxy enforces.
type Server struct {
	Name          string `json:"name"`
	Group         string `json:"group"`
	Host          string `json:"host"`
	Port          int32  `json:"port"`
	MaintenanceDB string `json:"maintenanceDB"`
	Username      string `json:"username"`
	PassFile      string `json:"passFile"`
	SSLMode       string `json:"sslMode"`
	// Password is written into the pgpass file, never into the server
	// definition, so it stays out of pgAdmin's settings database.
	Password string `json:"password"`
}

// Syncer applies the requested state to pgAdmin.
type Syncer interface {
	Sync(context.Context, Request) error
}

// Disabled is the no-op Syncer used when admin sync is not configured.
type Disabled struct{}

// Sync implements Syncer without contacting the Pod.
func (Disabled) Sync(context.Context, Request) error { return nil }

// SidecarSecretName returns the name of the per-console admin-sync Secret.
func SidecarSecretName(consoleName string) string {
	return consoleName + "-pgconsole-pgadmin-sync"
}

// selectReadyPod picks a Ready pod annotated with the requested
// configuration checksum, in name order for a deterministic choice. Pods at
// another revision are skipped because syncing through them would apply the
// role mapping against a stale configuration; with no eligible pod it
// errors so the caller retries later.
func selectReadyPod(pods []corev1.Pod, checksum string) (*corev1.Pod, error) {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for i := range pods {
		pod := &pods[i]
		if pod.Annotations[pgtoolboxv1alpha1.ConfigChecksumAnnotation] == checksum && podReady(pod) {
			return pod, nil
		}
	}
	return nil, fmt.Errorf("no ready pgAdmin pod found at requested configuration revision")
}

// podReady reports whether the pod is Running with a True Ready condition,
// the only state in which its admin-sync sidecar is safe to call.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
