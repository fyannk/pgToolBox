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

package admissionpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Paths the operator serves the two webhooks on (the OLM CSV's webhookdefinitions).
const (
	ResourcesPath     = "/validate-pgtoolbox-resources"
	ClusterDeletePath = "/validate-cnpg-cluster-delete"
)

// The CEL policies' messages, word for word (they name Secrets and tokens,
// they hold none: gosec G101 is a false positive here).
//
//nolint:gosec // messages, not credentials
const (
	msgImage            = "spec.image may be set only by the users and groups the platform team listed in the operator chart (operator.admissionPolicy.imageOverride); omit it to use the operator's default image"
	msgWorkloadIdentity = "spec.workloadIdentity may be set only by the users and groups the platform team listed in the operator chart (operator.admissionPolicy.workloadIdentity)"
	msgSecretRefs       = "a PgToolBoxConsole, a PgAdmin or an EvidenceAgent requires get on every Secret it references, on create and on any change of its spec; one that references more than two requires get on every Secret of the namespace"
	msgCredentials      = "a PgAdmin that offers the credentials of Clusters of its namespace (spec.credentials with clusterRefs or clusterSelector) or keeps its settings on one (spec.configDatabase) requires get on the Secrets of the namespace, on create and on any change of its spec"
	msgProjectedToken   = "a PgToolBoxConsole that sends its projected ServiceAccount token to Prometheus (spec.prometheus.projectedToken) requires create on serviceaccounts/token for the console's ServiceAccount, on create and on any change of its spec"
	msgExposure         = "annotations on the exposure object or the Service (spec.exposure.annotations, metadataPropagation for All, Exposure or Service) require create on the exposure kind (ingresses, routes or httproutes) in the namespace, on create and on any change of its spec"
	msgDrill            = "a ServiceAccount holding the restore-drill teardown right may delete only the restore drill Clusters it created (drill-owner label, drill label, drill seal)"
)

// secretRef is one Secret reference the CEL's secretRefs variable reads: the
// name of the reference at path below spec, or of each element of the list
// there (bare: the element is the name).
type secretRef struct {
	path       string
	list, bare bool
}

// secretRefs is the CEL's secretRefs, per resource (webhook_test.go
// compares it with the chart's policy).
var secretRefs = map[string][]secretRef{
	"pgtoolboxconsoles": {
		{path: "auth.oidc.clientSecretRef"},
		{path: "auth.oidc.grantClient.clientSecretRef"},
		{path: "auth.openshift.clientSecretRef"},
		{path: "prometheus.tokenSecretRef"},
		{path: "digest.delivery.chatWebhookSecretRef"},
		{path: "digest.delivery.webhookSecretRef"},
		{path: "digest.delivery.smtp.passwordSecretRef"},
		{path: "tracing.headersSecretRef"},
		{path: "restoreDrills.sealKeySecretRef"},
		{path: "exposure.tls.certificateSecretRef"},
		{path: "image.pullSecrets", list: true},
	},
	"pgadmins": {
		{path: "auth.oauth2.clientSecretRef"},
		{path: "auth.openshiftOAuthProxy.image.pullSecrets", list: true},
		{path: "exposure.tls.certificateSecretRef"},
		{path: "image.pullSecrets", list: true},
	},
	"evidenceagents": {{path: "allowedSecrets", list: true, bare: true}},
}

// Validator is the operator chart's two ValidatingAdmissionPolicies
// (pgtoolbox-image-override and pgtoolbox-console-restore-drills) as an
// admission webhook, for installs that cannot ship the policies (OLM). The
// rules, their order and their messages are the CEL's; the authorizer
// checks are SubjectAccessReviews for the request's user.
type Validator struct {
	ImageUsers, ImageGroups, IdentityUsers, IdentityGroups []string
	// Authorize answers a SubjectAccessReview.
	Authorize func(context.Context, authorizationv1.SubjectAccessReviewSpec) (bool, error)
}

// SubjectAccessReviews is Validator.Authorize over a client.
func SubjectAccessReviews(c client.Client) func(context.Context, authorizationv1.SubjectAccessReviewSpec) (bool, error) {
	return func(ctx context.Context, spec authorizationv1.SubjectAccessReviewSpec) (bool, error) {
		review := &authorizationv1.SubjectAccessReview{Spec: spec}
		if err := c.Create(ctx, review); err != nil {
			return false, fmt.Errorf("subjectaccessreview: %w", err)
		}
		return review.Status.Allowed, nil
	}
}

// Handler serves fn: a refusal is the rule's message, an error a 500 (which
// failurePolicy Fail turns into a refusal too).
func Handler(fn func(context.Context, admission.Request) (string, error)) admission.Handler {
	return admission.HandlerFunc(func(ctx context.Context, req admission.Request) admission.Response {
		refusal, err := fn(ctx, req)
		switch {
		case err != nil:
			return admission.Errored(http.StatusInternalServerError, err)
		case refusal != "":
			return admission.ValidationResponse(false, refusal)
		}
		return admission.Allowed("")
	})
}

// CustomResource is pgtoolbox-image-override: CREATE and UPDATE of
// PgToolBoxConsoles, PgAdmins and EvidenceAgents.
func (v *Validator) CustomResource(ctx context.Context, req admission.Request) (string, error) {
	object, err := decode(req.Object.Raw)
	if err != nil {
		return "", err
	}
	old, err := decode(req.OldObject.Raw)
	if err != nil {
		return "", err
	}
	resource, user := req.Resource.Resource, req.UserInfo
	spec, hasSpec := lookup(object, "spec")
	oldSpec, oldHasSpec := lookup(old, "spec")
	kept := func(path string) bool { // set on the old object, and unchanged
		was, ok := lookup(old, "spec."+path)
		now, _ := lookup(object, "spec."+path)
		return old != nil && ok && reflect.DeepEqual(was, now)
	}
	if _, ok := lookup(object, "spec.image"); ok && resource != "pgtoolboxconsoles" &&
		!kept("image") && !listed(user, v.ImageUsers, v.ImageGroups) {
		return msgImage, nil
	}
	if _, ok := lookup(object, "spec.workloadIdentity"); ok && resource == "evidenceagents" &&
		!kept("workloadIdentity") && !listed(user, v.IdentityUsers, v.IdentityGroups) {
		return msgWorkloadIdentity, nil
	}
	if old != nil && hasSpec == oldHasSpec && reflect.DeepEqual(spec, oldSpec) {
		return "", nil // the spec did not change: the other rules do not apply
	}
	ns, name := req.Namespace, nameOf(object)
	can := func(group, resource, subresource, name, verb string) (bool, error) {
		return v.Authorize(ctx, authorizationv1.SubjectAccessReviewSpec{
			User: user.Username, Groups: user.Groups, UID: user.UID, Extra: extra(user.Extra),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns, Verb: verb, Group: group, Resource: resource, Subresource: subresource, Name: name,
			},
		})
	}

	names := referencedSecrets(object, resource)
	if len(names) > 2 {
		names = []string{""} // one check on every Secret of the namespace
	}
	for _, secret := range names {
		if ok, err := can("", "secrets", "", secret, "get"); err != nil || !ok {
			return msgSecretRefs, err
		}
	}
	if resource == "pgadmins" && offersCredentials(object) {
		if ok, err := can("", "secrets", "", "", "get"); err != nil || !ok {
			return msgCredentials, err
		}
	}
	if projected, _ := lookup(object, "spec.prometheus.projectedToken"); resource == "pgtoolboxconsoles" && projected == true {
		account := name
		if !strings.Contains(name, "console") {
			account += "-console"
		}
		if ok, err := can("", "serviceaccounts", "token", account, "create"); err != nil || !ok {
			return msgProjectedToken, err
		}
	}
	exposure, _ := lookup(object, "spec.exposure.type")
	if exposure == nil || resource == "evidenceagents" {
		exposure = "clusterIP"
	}
	if exposure != "clusterIP" && annotatesExposure(object) {
		group, kind := "networking.k8s.io", "ingresses"
		switch exposure {
		case "route":
			group, kind = "route.openshift.io", "routes"
		case "gateway":
			group, kind = "gateway.networking.k8s.io", "httproutes"
		}
		if ok, err := can(group, kind, "", "", "create"); err != nil || !ok {
			return msgExposure, err
		}
	}
	return "", nil
}

// ClusterDelete is pgtoolbox-console-restore-drills: a ServiceAccount that
// holds teardown on restoredrills in the namespace, and not every verb, may
// delete only a sealed drill Cluster of that namespace.
func (v *Validator) ClusterDelete(ctx context.Context, req admission.Request) (string, error) {
	user := req.UserInfo
	if !strings.HasPrefix(user.Username, "system:serviceaccount:") {
		return "", nil
	}
	can := func(verb string) (bool, error) {
		return v.Authorize(ctx, authorizationv1.SubjectAccessReviewSpec{
			User: user.Username, Groups: user.Groups, UID: user.UID, Extra: extra(user.Extra),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: req.Namespace, Verb: verb, Group: "pgtoolbox.fyannk.dev", Resource: "restoredrills",
			},
		})
	}
	if teardown, err := can("teardown"); err != nil || !teardown {
		return "", err
	}
	if administrator, err := can("*"); err != nil || administrator {
		return "", err
	}
	old, err := decode(req.OldObject.Raw)
	if err != nil {
		return "", err
	}
	labels, _ := lookup(old, "metadata.labels")
	annotations, _ := lookup(old, "metadata.annotations")
	l, _ := labels.(map[string]any)
	a, _ := annotations.(map[string]any)
	_, drill := l["pgtoolbox.fyannk.dev/restore-drill-of"]
	_, sealed := a["pgtoolbox.fyannk.dev/restore-drill-seal"]
	if drill && sealed && l["pgtoolbox.fyannk.dev/drill-owner"] == req.Namespace {
		return "", nil
	}
	return msgDrill, nil
}

func decode(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode the request's object: %w", err)
	}
	return object, nil
}

// lookup is CEL's has() and the value, at a dotted path.
func lookup(object map[string]any, path string) (any, bool) {
	var value any = object
	for _, key := range strings.Split(path, ".") {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		if value, ok = m[key]; !ok {
			return nil, false
		}
	}
	return value, true
}

func nameOf(object map[string]any) string {
	name, _ := lookup(object, "metadata.name")
	s, _ := name.(string)
	return s
}

func listed(user authenticationv1.UserInfo, users, groups []string) bool {
	return slices.Contains(users, user.Username) || slices.ContainsFunc(user.Groups, func(g string) bool { return slices.Contains(groups, g) })
}

func extra(in map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	out := make(map[string]authorizationv1.ExtraValue, len(in))
	for k, v := range in {
		out[k] = authorizationv1.ExtraValue(v)
	}
	return out
}

// referencedSecrets is the CEL's secretRefs: one name per reference, in
// order, duplicates kept.
func referencedSecrets(object map[string]any, resource string) []string {
	var names []string
	add := func(v any, bare bool) {
		if !bare {
			m, _ := v.(map[string]any)
			v = m["name"]
		}
		s, _ := v.(string)
		names = append(names, s)
	}
	for _, ref := range secretRefs[resource] {
		value, ok := lookup(object, "spec."+ref.path)
		switch {
		case !ok:
		case ref.list:
			items, _ := value.([]any)
			for _, item := range items {
				add(item, ref.bare)
			}
		default:
			add(value, false)
		}
	}
	return names
}

// offersCredentials: configDatabase, or Clusters named or selected with a
// credential offer the defaults keep on.
func offersCredentials(object map[string]any) bool {
	if _, ok := lookup(object, "spec.configDatabase"); ok {
		return true
	}
	refs, _ := lookup(object, "spec.clusterRefs")
	_, selector := lookup(object, "spec.clusterSelector")
	if items, _ := refs.([]any); len(items) == 0 && !selector {
		return false
	}
	on := func(key string, absent bool) bool {
		v, ok := lookup(object, "spec.credentials."+key)
		return (!ok && absent) || v == true
	}
	_, hasCredentials := lookup(object, "spec.credentials")
	return !hasCredentials || on("application", true) || on("databaseRoles", true) || on("superuser", false)
}

// annotatesExposure: annotations on the exposure object, or propagated to
// it or to the Service.
func annotatesExposure(object map[string]any) bool {
	if a, _ := lookup(object, "spec.exposure.annotations"); len(asMap(a)) > 0 {
		return true
	}
	rules, _ := lookup(object, "spec.metadataPropagation")
	items, _ := rules.([]any)
	return slices.ContainsFunc(items, func(rule any) bool {
		r := asMap(rule)
		return slices.Contains([]any{"All", "Exposure", "Service"}, r["target"]) && len(asMap(r["annotations"])) > 0
	})
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
