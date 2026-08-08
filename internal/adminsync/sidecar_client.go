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

package adminsync

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Secret keys of the operator-generated admin-sync material.
const (
	SidecarSecretCAKey    = "ca.crt"
	SidecarSecretCertKey  = "tls.crt"
	SidecarSecretKeyKey   = "tls.key"
	SidecarSecretTokenKey = "token"
)

// SidecarClient applies pgAdmin state by calling the admin-sync API served
// inside the pgAdmin Pod. The connection pins the operator-generated CA and
// authenticates with the shared bearer token; both are read live from the
// per-console Secret.
type SidecarClient struct {
	client  kubernetes.Interface
	reader  client.Reader
	timeout time.Duration
}

// NewSidecarClient constructs the production sidecar-mode Syncer.
func NewSidecarClient(config *rest.Config, reader client.Reader) (*SidecarClient, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("construct admin sync client: %w", err)
	}
	// One sync is a batch of setup.py invocations, and each one boots a
	// Flask application and runs the settings-database migrations before it
	// does any work. Thirty seconds was a network-call timeout applied to
	// something that is not a network call: on a loaded node a single sync
	// exceeded it and every user stayed unprovisioned. This bounds a
	// pod-local request, so it can afford to wait.
	return &SidecarClient{client: clientset, reader: reader, timeout: 5 * time.Minute}, nil
}

// Sync selects the ready pgAdmin Pod at the requested checksum and posts
// the complete desired state to its admin-sync API.
func (c *SidecarClient) Sync(ctx context.Context, request Request) error {
	pod, err := c.readyPod(ctx, request)
	if err != nil {
		return err
	}
	if pod.Status.PodIP == "" {
		return fmt.Errorf("pgAdmin pod %s/%s has no IP yet", pod.Namespace, pod.Name)
	}

	var secret corev1.Secret
	secretKey := client.ObjectKey{
		Namespace: request.Namespace,
		Name:      SidecarSecretName(request.ConsoleName),
	}
	if err := c.reader.Get(ctx, secretKey, &secret); err != nil {
		return fmt.Errorf("read admin-sync secret: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(secret.Data[SidecarSecretCAKey]) {
		return fmt.Errorf("admin-sync secret %s/%s has no usable CA", secretKey.Namespace, secretKey.Name)
	}
	token := strings.TrimSpace(string(secret.Data[SidecarSecretTokenKey]))

	body, err := json.Marshal(SyncRequest{Users: request.Users})
	if err != nil {
		return fmt.Errorf("encode admin-sync request: %w", err)
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool,
		// Pods are dialed by IP; the generated certificate carries the
		// fixed DNS SAN instead.
		ServerName: SidecarServerName,
		MinVersion: tls.VersionTLS12,
	}}
	httpClient := &http.Client{Transport: transport, Timeout: c.timeout}
	url := fmt.Sprintf("https://%s:%d/v1/sync", pod.Status.PodIP, SidecarPort)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("call pgAdmin admin-sync API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("pgAdmin admin-sync API returned %d: %s", response.StatusCode, string(message))
	}
	return nil
}

// readyPod lists the console's pods live through the API server and picks
// the one ready at the requested checksum, so the sync never targets a
// replica still running an older configuration.
func (c *SidecarClient) readyPod(ctx context.Context, request Request) (*corev1.Pod, error) {
	pods, err := c.client.CoreV1().Pods(request.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(request.Selector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list pgAdmin pods for admin sync: %w", err)
	}
	return selectReadyPod(pods.Items, request.Checksum)
}
