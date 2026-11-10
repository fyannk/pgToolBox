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
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

func keyRef(name string) *pgtoolboxv1alpha1.SecretKeyReference {
	return &pgtoolboxv1alpha1.SecretKeyReference{Name: name, Key: "k"}
}

func localRef(name string) *pgtoolboxv1alpha1.LocalObjectReference {
	return &pgtoolboxv1alpha1.LocalObjectReference{Name: name}
}

// everyReferenceConsole references a Secret and a ConfigMap from every
// field that can.
func everyReferenceConsole(provider pgtoolboxv1alpha1.PgToolBoxConsoleAuthProvider) *pgtoolboxv1alpha1.PgToolBoxConsole {
	c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}}
	c.Spec.Auth.Provider = provider
	c.Spec.Auth.OIDC = &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec{
		IssuerURL: "https://idp.example", ClientID: "console",
		ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "oidc-client"},
		CAConfigMapRef:  localRef("oidc-ca"),
		GrantClient: &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCClient{
			ClientID: "console-grants", ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "oidc-grant-client"},
		},
	}
	c.Spec.Auth.OpenShift = &pgtoolboxv1alpha1.PgToolBoxConsoleOpenShiftSpec{ClientSecretRef: keyRef("openshift-client"), CAConfigMapRef: localRef("openshift-ca")}
	c.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{
		URL: "https://prometheus.example", TokenSecretRef: keyRef("prom-token"), CAConfigMapRef: localRef("prom-ca"),
	}
	c.Spec.InstanceCABundleRef = localRef("instance-bundle")
	c.Spec.LifecycleConfigMapRef = localRef("eol-table")
	c.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
		ChatWebhookSecretRef: keyRef("chat"),
		WebhookSecretRef:     keyRef("webhook"),
		SMTP:                 &pgtoolboxv1alpha1.PgToolBoxConsoleDigestSMTPSpec{Address: "smtp.example:587", Username: "digest", PasswordSecretRef: keyRef("smtp")},
	}
	c.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{Endpoint: "http://collector.example:4318", HeadersSecretRef: keyRef("tracing-headers")}
	c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true, SealKeySecretRef: keyRef("drill-seal")}
	return c
}

func refNames(refs []objectReference) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

func TestReferencedSecrets(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		console *pgtoolboxv1alpha1.PgToolBoxConsole
		want    []string
	}{
		"nothing referenced": {console: &pgtoolboxv1alpha1.PgToolBoxConsole{}},
		"token sign-in mounts no provider Secret": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken),
			want:    []string{"prom-token", "chat", "webhook", "smtp", "tracing-headers", "drill-seal"},
		},
		"oidc": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC),
			want:    []string{"oidc-client", "oidc-grant-client", "prom-token", "chat", "webhook", "smtp", "tracing-headers", "drill-seal"},
		},
		"openshift": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift),
			want:    []string{"openshift-client", "prom-token", "chat", "webhook", "smtp", "tracing-headers", "drill-seal"},
		},
		"smtp without a username mounts no password": {
			console: func() *pgtoolboxv1alpha1.PgToolBoxConsole {
				c := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
				c.Spec.Digest.Delivery.SMTP.Username = ""
				c.Spec.Prometheus.TokenSecretRef = nil
				c.Spec.Tracing = nil
				c.Spec.RestoreDrills.SealKeySecretRef = nil
				return c
			}(),
			want: []string{"chat", "webhook"},
		},
		"a drill seal key with drills off mounts nothing": {
			console: func() *pgtoolboxv1alpha1.PgToolBoxConsole {
				c := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
				c.Spec.RestoreDrills.Enabled = false
				return c
			}(),
			want: []string{"prom-token", "chat", "webhook", "smtp", "tracing-headers"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := refNames(referencedSecrets(tc.console)); !slices.Equal(got, tc.want) {
				t.Fatalf("referencedSecrets = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReferencedConfigMaps(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		console *pgtoolboxv1alpha1.PgToolBoxConsole
		want    []string
	}{
		"nothing referenced: the operator keeps both its ConfigMaps": {console: &pgtoolboxv1alpha1.PgToolBoxConsole{}},
		"token sign-in mounts no provider CA": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken),
			want:    []string{"prom-ca", "instance-bundle", "eol-table"},
		},
		"oidc": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC),
			want:    []string{"oidc-ca", "prom-ca", "instance-bundle", "eol-table"},
		},
		"openshift": {
			console: everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift),
			want:    []string{"prom-ca", "openshift-ca", "instance-bundle", "eol-table"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := refNames(referencedConfigMaps(tc.console)); !slices.Equal(got, tc.want) {
				t.Fatalf("referencedConfigMaps = %v, want %v", got, tc.want)
			}
		})
	}
}

// platformConfigMaps are published in every namespace by the platform and
// never checked.
var platformConfigMaps = []string{"kube-root-ca.crt", "openshift-service-ca.crt"}

// mountedObjects lists the Secrets and ConfigMaps the Pod's volumes name
// (projected sources included), and the ConfigMaps among them mounted
// without optional.
func mountedObjects(volumes []corev1.Volume) (secrets, configMaps, required []string) {
	configMap := func(name string, optional *bool) {
		if slices.Contains(platformConfigMaps, name) {
			return
		}
		configMaps = append(configMaps, name)
		if optional == nil || !*optional {
			required = append(required, name)
		}
	}
	for _, volume := range volumes {
		switch {
		case volume.Secret != nil:
			secrets = append(secrets, volume.Secret.SecretName)
		case volume.ConfigMap != nil:
			configMap(volume.ConfigMap.Name, volume.ConfigMap.Optional)
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil {
					secrets = append(secrets, source.Secret.Name)
				}
				if source.ConfigMap != nil {
					configMap(source.ConfigMap.Name, source.ConfigMap.Optional)
				}
			}
		}
	}
	return secrets, configMaps, required
}

func sorted(names []string) []string {
	names = slices.Clone(names)
	slices.Sort(names)
	return names
}

// TestReferencesAreTheMountedOnes keeps the check and the builder
// together: with every reference set, each user Secret and ConfigMap
// consoleVolumes mounts is checked, and nothing else is; with none set,
// every ConfigMap mounted without optional is still checked (none is: the
// operator's own are optional), so a new required mount cannot skip the
// table.
func TestReferencesAreTheMountedOnes(t *testing.T) {
	t.Parallel()
	for _, provider := range []pgtoolboxv1alpha1.PgToolBoxConsoleAuthProvider{
		pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken, pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC, pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift,
	} {
		c := everyReferenceConsole(provider)
		_, volumes := consoleVolumes(c)
		secrets, configMaps, required := mountedObjects(volumes)
		secrets = slices.DeleteFunc(secrets, func(name string) bool { return name == c.SessionSecretName() })
		if got, want := sorted(refNames(referencedSecrets(c))), sorted(secrets); !slices.Equal(got, want) {
			t.Errorf("%s: Secrets mounted %v, checked %v", provider, want, got)
		}
		checked := refNames(referencedConfigMaps(c))
		if got, want := sorted(checked), sorted(configMaps); !slices.Equal(got, want) {
			t.Errorf("%s: ConfigMaps mounted %v, checked %v", provider, want, got)
		}
		for _, name := range required {
			if !slices.Contains(checked, name) {
				t.Errorf("%s: ConfigMap %s is mounted without optional and not checked", provider, name)
			}
		}

		bare := c.DeepCopy()
		bare.Spec.InstanceCABundleRef, bare.Spec.LifecycleConfigMapRef = nil, nil
		_, volumes = consoleVolumes(bare)
		_, _, required = mountedObjects(volumes)
		checked = refNames(referencedConfigMaps(bare))
		for _, name := range required {
			if !slices.Contains(checked, name) {
				t.Errorf("%s without bundle or lifecycle refs: ConfigMap %s is mounted without optional and not checked", provider, name)
			}
		}
	}
}

func userSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}}
}

func userConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}}
}

func TestCheckReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := cnpgScheme(t)

	t.Run("all present", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).
			WithObjects(userSecret("prom-token"), userSecret("chat"), userSecret("webhook"), userSecret("smtp"), userSecret("tracing-headers"), userSecret("drill-seal"),
				userConfigMap("prom-ca"), userConfigMap("instance-bundle"), userConfigMap("eol-table")).Build()
		r := &Reconciler{APIReader: reader}
		console := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
		if err := r.checkReferences(ctx, console); err != nil {
			t.Fatalf("checkReferences = %v, want nil", err)
		}
		if meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid) != nil {
			t.Fatal("a passing check set ConfigurationValid; the caller does")
		}
	})

	t.Run("missing ones stall, each named with its field", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).WithObjects(userSecret("chat"), userSecret("smtp")).Build()
		r := &Reconciler{APIReader: reader}
		console := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
		err := r.checkReferences(ctx, console)
		var stalled *reconcile.StallError
		if !errors.As(err, &stalled) || stalled.Reason != pgtoolboxv1alpha1.ReasonSecretNotFound {
			t.Fatalf("checkReferences = %v, want a SecretNotFound stall", err)
		}
		condition := meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != pgtoolboxv1alpha1.ReasonSecretNotFound.String() {
			t.Fatalf("ConfigurationValid = %+v, want False/SecretNotFound", condition)
		}
		for _, want := range []string{
			"spec.prometheus.tokenSecretRef: Secret prom-token",
			"spec.digest.delivery.webhookSecretRef: Secret webhook",
			"spec.tracing.headersSecretRef: Secret tracing-headers",
			"namespace team-a",
		} {
			if !strings.Contains(condition.Message, want) {
				t.Errorf("message %q lacks %q", condition.Message, want)
			}
		}
		if strings.Contains(condition.Message, "Secret chat") {
			t.Errorf("message %q names a Secret that exists", condition.Message)
		}
	})

	t.Run("Secrets present, missing ConfigMaps stall with ConfigMapNotFound", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).
			WithObjects(userSecret("oidc-client"), userSecret("oidc-grant-client"), userSecret("prom-token"), userSecret("chat"), userSecret("webhook"), userSecret("smtp"), userSecret("tracing-headers"), userSecret("drill-seal"),
				userConfigMap("prom-ca")).Build()
		r := &Reconciler{APIReader: reader}
		console := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC)
		err := r.checkReferences(ctx, console)
		var stalled *reconcile.StallError
		if !errors.As(err, &stalled) || stalled.Reason != pgtoolboxv1alpha1.ReasonConfigMapNotFound {
			t.Fatalf("checkReferences = %v, want a ConfigMapNotFound stall", err)
		}
		condition := meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != pgtoolboxv1alpha1.ReasonConfigMapNotFound.String() {
			t.Fatalf("ConfigurationValid = %+v, want False/ConfigMapNotFound", condition)
		}
		for _, want := range []string{
			"spec.auth.oidc.caConfigMapRef: ConfigMap oidc-ca",
			"spec.instanceCABundleRef: ConfigMap instance-bundle",
			"spec.lifecycleConfigMapRef: ConfigMap eol-table",
		} {
			if !strings.Contains(condition.Message, want) {
				t.Errorf("message %q lacks %q", condition.Message, want)
			}
		}
		if strings.Contains(condition.Message, "prom-ca") {
			t.Errorf("message %q names a ConfigMap that exists", condition.Message)
		}
	})

	t.Run("an API error is returned, not reported as missing", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "chat", errors.New("denied"))
			},
		}).Build()
		r := &Reconciler{APIReader: reader}
		console := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
		err := r.checkReferences(ctx, console)
		var stalled *reconcile.StallError
		if err == nil || errors.As(err, &stalled) || !apierrors.IsForbidden(err) {
			t.Fatalf("checkReferences = %v, want the Forbidden error", err)
		}
	})
}

func TestForSecret(t *testing.T) {
	t.Parallel()
	mounting := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
	mounting.Spec.InstanceCABundleRef = nil // mirrors, as other does
	// other mirrors the instance CAs and also mounts a CA Secret as a
	// Prometheus token: one request for it, not two.
	other := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-a"}}
	other.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{URL: "https://prometheus.example", TokenSecretRef: keyRef("pg-a-ca")}
	elsewhere := everyReferenceConsole(pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken)
	elsewhere.Namespace = "team-b"
	c := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).
		WithObjects(mounting, other, elsewhere, testCluster("pg-a", nil, "")).
		WithIndex(&pgtoolboxv1alpha1.PgToolBoxConsole{}, indexReferencedSecret, referencedSecretNames).
		WithIndex(&cnpgv1.Cluster{}, indexServerCASecret, func(object client.Object) []string {
			return []string{serverCASecretName(object.(*cnpgv1.Cluster))}
		}).Build()
	r := &Reconciler{Client: c}
	ctx := context.Background()

	if requests := r.forSecret(ctx, userSecret("webhook")); len(requests) != 1 || requests[0].Name != "console" || requests[0].Namespace != "team-a" {
		t.Fatalf("webhook Secret enqueued %v, want team-a/console only", requests)
	}
	if requests := r.forSecret(ctx, userSecret("app-credentials")); len(requests) != 0 {
		t.Fatalf("an unreferenced Secret enqueued %v", requests)
	}
	// A server CA Secret reaches the mirroring consoles (both mirror, by
	// default), once each.
	requests := r.forSecret(ctx, userSecret("pg-a-ca"))
	if len(requests) != 2 {
		t.Fatalf("CA Secret enqueued %v, want the two mirroring consoles", requests)
	}
}
