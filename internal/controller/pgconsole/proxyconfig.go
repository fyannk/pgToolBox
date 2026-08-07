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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	proxyconfig "github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

// The proxy configuration Secret carries two keys: the rendered
// configuration file the proxy reads, and the session key it was rendered
// from. The key is kept as its own entry so a steady-state reconcile reuses
// it without parsing it back out of the rendered file — session material is
// generated once per PgConsole and never rotated silently.
const (
	proxyConfigSecretSuffix = "-proxy"
	proxyConfigFileName     = "config.yaml"
	// #nosec G101 -- key name only; no secret material is embedded.
	proxySessionKeyName = "session-key"

	// proxyConfigMountPath is where the configuration Secret is mounted in
	// the proxy container; the file name is fixed by proxyConfigFileName.
	// #nosec G101 -- mount path only; no secret material is embedded.
	proxyConfigMountPath = "/etc/pgtoolbox-proxy"

	// The OIDC client secret reaches the proxy container as a file under a
	// fixed name, whatever the referenced Secret calls its key.
	// #nosec G101 -- mount path only; no secret material is embedded.
	oidcClientSecretMountPath = "/etc/pgtoolbox-proxy/oidc"
	oidcClientSecretFile      = "client-secret"
	// defaultOIDCClientSecretKey is read from the referenced Secret when the
	// reference names no key.
	// #nosec G101 -- key name only; no secret material is embedded.
	defaultOIDCClientSecretKey = "clientSecret"

	proxyCallbackPath     = "/auth/oidc/callback"
	openshiftCallbackPath = "/auth/openshift/callback"
)

// configIssue is the typed "why not" for the ProxyConfigReady condition: an
// expected outcome of an unsatisfiable or not-yet-complete configuration,
// reported instead of deploying a proxy that cannot start.
type configIssue struct {
	reason  string
	message string
}

func (i *configIssue) Error() string { return i.message }

func issue(reason, format string, args ...any) *configIssue {
	return &configIssue{reason: reason, message: fmt.Sprintf(format, args...)}
}

// renderProxyConfig renders the proxy configuration file from the spec and
// the resolved console users. Only the oidc and local provider modes render;
// openshift is rejected because this build of the proxy does not implement it.
// The rendered file is round-tripped through the proxy's own strict parser so
// the controller can never write a configuration the proxy would refuse at
// startup.
func renderProxyConfig(
	console *pgtoolboxv1alpha1.PgConsole,
	sessionKey string,
	pgAdminEnabled bool,
	users []proxyconfig.User,
) ([]byte, error) {
	cfg := proxyconfig.Config{
		Session: proxyconfig.SessionConfig{
			CookieSecrets: []string{sessionKey},
		},
		Server: proxyconfig.ServerConfig{
			Listen: ":" + portString(proxyPort),
		},
		Users: users,
		Routes: []proxyconfig.RouteConfig{
			{
				PathPrefix: "/",
				Upstream:   loopbackUpstream(consolePort),
				MinLevel:   proxyconfig.LevelView,
			},
		},
		AccessRequest: proxyconfig.AccessRequestConfig{
			Enabled:     true,
			ConsoleName: console.Name,
			Namespace:   console.Namespace,
		},
	}

	switch mode := console.Spec.Proxy.Authentication.Mode; mode {
	case pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC:
		oidc := console.Spec.Proxy.Authentication.OIDC
		if oidc == nil {
			return nil, fmt.Errorf("spec.proxy.authentication.oidc is required when mode is oidc")
		}
		cfg.Provider = proxyconfig.ProviderConfig{
			Mode: proxyconfig.ModeOIDC,
			OIDC: &proxyconfig.OIDCConfig{
				IssuerURL:        oidc.IssuerURL,
				ClientID:         oidc.ClientID,
				ClientSecretFile: oidcClientSecretMountPath + "/" + oidcClientSecretFile,
				RedirectURL:      consoleBaseURL(console) + proxyCallbackPath,
			},
		}
	case pgtoolboxv1alpha1.ProxyAuthenticationModeLocal:
		cfg.Provider = proxyconfig.ProviderConfig{Mode: proxyconfig.ModeLocal}
	case pgtoolboxv1alpha1.ProxyAuthenticationModeOpenShift:
		cfg.Provider = proxyconfig.ProviderConfig{
			Mode: proxyconfig.ModeOpenShift,
			OpenShift: &proxyconfig.OpenShiftConfig{
				ClientID:         serviceAccountClientID(console),
				ClientSecretFile: serviceAccountRoot + "/token",
				RedirectURL:      consoleBaseURL(console) + openshiftCallbackPath,
				CAFile:           serviceAccountRoot + "/ca.crt",
			},
		}
	default:
		return nil, fmt.Errorf("proxy authentication mode %q is not supported by this build", mode)
	}

	if pgAdminEnabled {
		minLevel := console.Spec.PgAdmin.AccessMinLevel
		if minLevel == "" {
			minLevel = string(proxyconfig.LevelDBA)
		}
		if level := proxyconfig.Level(minLevel); !level.Valid() {
			return nil, fmt.Errorf("spec.pgAdmin.accessMinLevel %q is invalid", minLevel)
		} else {
			// Longest prefixes first so /pgadmin never falls through to /.
			cfg.Routes = append([]proxyconfig.RouteConfig{{
				PathPrefix: "/pgadmin",
				Upstream:   loopbackUpstream(pgAdminPort),
				MinLevel:   level,
			}}, cfg.Routes...)
		}
	}

	rendered, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render proxy configuration: %w", err)
	}
	if _, _, err := proxyconfig.Parse(rendered); err != nil {
		return nil, fmt.Errorf("rendered proxy configuration failed validation: %w", err)
	}
	return rendered, nil
}

// loopbackUpstream is the in-Pod upstream URL for one container port.
// Identity headers are trusted only because every upstream is loopback.
func loopbackUpstream(port int32) string {
	return "http://127.0.0.1:" + portString(port)
}

func portString(port int32) string {
	return fmt.Sprintf("%d", port)
}

// consoleBaseURL is the external base URL of the console, from which the
// OIDC and OpenShift redirect URLs derive. A clusterIP-only console has no
// external URL; it falls back to the loopback listener so port-forward flows
// keep working.
func consoleBaseURL(console *pgtoolboxv1alpha1.PgConsole) string {
	if hostname := console.Spec.Exposure.Hostname; hostname != "" {
		return "https://" + hostname
	}
	return "http://localhost:" + portString(proxyPort)
}

// serviceAccountClientID returns the OpenShift OAuth client ID of the
// console's service account.
func serviceAccountClientID(console *pgtoolboxv1alpha1.PgConsole) string {
	return "system:serviceaccount:" + console.Namespace + ":" + application.ResourceName(console.Name, "")
}

// reconcileProxyConfigSecret converges the proxy configuration Secret and
// returns the checksum of the rendered configuration. A nil issue with nil
// error is the success path; a non-nil issue is an expected gap to publish
// on ProxyConfigReady instead of reconciling further.
//
// Session-key stability: an existing Secret's session key is reused as-is,
// so cookies survive every reconcile. The key is regenerated in place only
// when the Secret exists but holds no usable key — self-healing a broken
// object, at the price of invalidating sessions, never a silent rotation.
// Reads go through APIReader because Secret content is never held in the
// informer cache.
func (r *Reconciler) reconcileProxyConfigSecret(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	proxyUsers []proxyconfig.User,
) (string, *configIssue, error) {
	if whyNot := r.checkOIDCClientSecret(ctx, console); whyNot != nil {
		return "", whyNot, nil
	}

	key := client.ObjectKey{
		Namespace: console.Namespace,
		Name:      application.ResourceName(console.Name, proxyConfigSecretSuffix),
	}
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, key, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", nil, err
	}
	exists := err == nil

	var sessionKey []byte
	if exists {
		sessionKey = existing.Data[proxySessionKeyName]
	}
	if len(sessionKey) == 0 {
		sessionKey, err = generateSessionKey()
		if err != nil {
			return "", nil, err
		}
	}

	rendered, err := renderProxyConfig(console, string(sessionKey), pgAdminEnabled(console), proxyUsers)
	if err != nil {
		return "", issue(pgtoolboxv1alpha1.ReasonRenderFailed, "%v", err), nil
	}
	checksum := render.Checksum(map[string][]byte{proxyConfigFileName: rendered})

	if exists {
		if owner := metav1.GetControllerOf(&existing); owner == nil || owner.UID != console.UID {
			return "", nil, fmt.Errorf("proxy configuration secret %s/%s exists and is not owned by this PgConsole",
				key.Namespace, key.Name)
		}
		if string(existing.Data[proxyConfigFileName]) == string(rendered) &&
			string(existing.Data[proxySessionKeyName]) == string(sessionKey) {
			return checksum, nil, nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[proxyConfigFileName] = rendered
		existing.Data[proxySessionKeyName] = sessionKey
		if err := r.Update(ctx, &existing); err != nil {
			return "", nil, fmt.Errorf("update proxy configuration secret %s/%s: %w", key.Namespace, key.Name, err)
		}
		return checksum, nil, nil
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			proxyConfigFileName: rendered,
			proxySessionKeyName: sessionKey,
		},
	}
	if err := controllerutil.SetControllerReference(console, secret, r.Scheme); err != nil {
		return "", nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		// The prior live read makes AlreadyExists a concurrent-reconcile
		// race only; surface it and let the next reconcile pick the
		// existing object up.
		return "", nil, fmt.Errorf("create proxy configuration secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return checksum, nil, nil
}

// checkOIDCClientSecret verifies the referenced client Secret and key exist
// before the workload mounts them, so a typo reports on ProxyConfigReady
// instead of surfacing as a Pod that cannot start.
func (r *Reconciler) checkOIDCClientSecret(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) *configIssue {
	auth := console.Spec.Proxy.Authentication
	if auth.Mode != pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC || auth.OIDC == nil {
		return nil
	}
	ref := auth.OIDC.ClientSecretRef
	var secret corev1.Secret
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: ref.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return issue(pgtoolboxv1alpha1.ReasonSecretNotFound,
			"OIDC client secret %s was not found", ref.Name)
	}
	if err != nil {
		return issue(pgtoolboxv1alpha1.ReasonSecretNotFound,
			"OIDC client secret %s could not be read: %v", ref.Name, err)
	}
	if len(secret.Data[oidcClientSecretKey(ref)]) == 0 {
		return issue(pgtoolboxv1alpha1.ReasonSecretKeyMissing,
			"OIDC client secret %s has no key %q", ref.Name, oidcClientSecretKey(ref))
	}
	return nil
}

// oidcClientSecretKey resolves the referenced key, applying the default.
func oidcClientSecretKey(ref pgtoolboxv1alpha1.SecretKeyReference) string {
	if ref.Key != "" {
		return ref.Key
	}
	return defaultOIDCClientSecretKey
}

// generateSessionKey draws the 32 random bytes the proxy derives its session
// sealing keys from, base64url-encoded so the value is a safe YAML string
// and environment-safe everywhere it may be copied.
func generateSessionKey() ([]byte, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("generate proxy session key: %w", err)
	}
	return []byte(base64.RawURLEncoding.EncodeToString(value)), nil
}
