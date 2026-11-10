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

package pgadmin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	authorizationv1 "github.com/openshift/api/authorization/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestOAuthProxySAR: the oauth-proxy posts --openshift-sar as is to
// authorization.openshift.io/v1 subjectaccessreviews, whose server drops a
// field it does not know. Decoded strictly into that API's Action, the review
// must name the PgAdmin's group, resource and name and ask for the connect
// verb: get is in the view role on OLM, and an unknown group field would
// leave the API group empty (a review of core-group pgadmins).
func TestOAuthProxySAR(t *testing.T) {
	p := openshiftPgAdmin()
	c := oauthProxyContainer(p, testInputs(p, true))
	var raw string
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--openshift-sar="); ok {
			if raw != "" {
				t.Fatal("two --openshift-sar arguments")
			}
			raw = v
		}
	}
	if raw == "" {
		t.Fatalf("no --openshift-sar in %v", c.Args)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var got authorizationv1.Action
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("--openshift-sar=%s is not an authorization.openshift.io/v1 Action: %v", raw, err)
	}
	want := authorizationv1.Action{
		Namespace:    p.Namespace,
		Verb:         "connect",
		Group:        pgtoolboxv1alpha1.GroupVersion.Group,
		Resource:     "pgadmins",
		ResourceName: p.Name,
	}
	if got.Namespace != want.Namespace || got.Verb != want.Verb || got.Group != want.Group ||
		got.Resource != want.Resource || got.ResourceName != want.ResourceName || got.Version != "" ||
		got.IsNonResourceURL || got.Path != "" {
		t.Fatalf("--openshift-sar = %+v, want %+v", got, want)
	}
	for _, viewOrEdit := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
		if got.Verb == viewOrEdit {
			t.Fatalf("the proxy asks for %q, which OLM's view or edit role carries on an owned CRD", got.Verb)
		}
	}
	if pgtoolboxv1alpha1.PgAdminConnectVerb != got.Verb {
		t.Fatalf("the proxy asks for %q, the console offers the link on %q", got.Verb, pgtoolboxv1alpha1.PgAdminConnectVerb)
	}
}

// proxyPostedReviews is what openshift/oauth-proxy does with --openshift-sar
// before it posts it (providers/openshift/provider.go, parseSubjectAccessReviews
// and encodeSARWithScope, master as of 2026-11): a value that is neither
// wrapped in [ ] becomes a one-element list, and each review gets
// "scopes": [] when it has none. Each element is POSTed as is, as the body of
// an authorization.openshift.io/v1 SubjectAccessReview (ReviewUser).
func proxyPostedReviews(t *testing.T, flag string) []map[string]any {
	t.Helper()
	flag = strings.TrimSpace(flag)
	if flag == "" {
		t.Fatal("empty --openshift-sar")
	}
	if flag[0] != '[' && flag[len(flag)-1] != ']' {
		flag = "[" + flag + "]"
	}
	var reviews []map[string]any
	if err := json.Unmarshal([]byte(flag), &reviews); err != nil {
		t.Fatalf("the proxy cannot parse --openshift-sar=%s: %v", flag, err)
	}
	for _, r := range reviews {
		if s, ok := r["scopes"].([]any); !ok || len(s) == 0 {
			r["scopes"] = []any{}
		}
	}
	return reviews
}

// TestOAuthProxySARIsASubjectAccessReview decodes the body the proxy posts
// strictly into the real authorization.openshift.io/v1 SubjectAccessReview:
// an unknown key (a misspelt resourceAPIGroup, a Kubernetes-style "group")
// fails here, where the API server would drop it silently and review the
// core group's "pgadmins".
func TestOAuthProxySARIsASubjectAccessReview(t *testing.T) {
	p := openshiftPgAdmin()
	reviews := proxyPostedReviews(t, proxySAR(p))
	if len(reviews) != 1 {
		t.Fatalf("the proxy would post %d reviews, want 1", len(reviews))
	}
	body, err := json.Marshal(reviews[0])
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var got authorizationv1.SubjectAccessReview
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the posted body %s is not an authorization.openshift.io/v1 SubjectAccessReview: %v", body, err)
	}
	if a := got.Action; a.Namespace != p.Namespace || a.Verb != pgtoolboxv1alpha1.PgAdminConnectVerb ||
		a.Group != pgtoolboxv1alpha1.GroupVersion.Group || a.Resource != "pgadmins" || a.ResourceName != p.Name {
		t.Fatalf("posted review = %+v", got.Action)
	}
	// The user is the bearer of the token the proxy sends, never named in
	// the body: a fixed user or group would review somebody else.
	if got.User != "" || len(got.GroupsSlice) != 0 || got.Scopes == nil || len(got.Scopes) != 0 {
		t.Fatalf("posted review names a subject or scopes: user %q groups %v scopes %v", got.User, got.GroupsSlice, got.Scopes)
	}

	// The strict decoder is what catches a wrong key: the Kubernetes
	// authorization/v1 spelling of the group field is refused.
	bad := strings.Replace(proxySAR(p), `"resourceAPIGroup"`, `"group"`, 1)
	body, _ = json.Marshal(proxyPostedReviews(t, bad)[0])
	dec = json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&authorizationv1.SubjectAccessReview{}); err == nil {
		t.Fatal(`a review with "group" decoded: the test would not catch an unknown key`)
	}
}

// TestOAuthProxySessionIsBounded: the proxy reviews access at sign-in only
// and keeps a session 168h by default; the rendered flag bounds it so a
// revoked connect (an expired access request) closes pgAdmin within the hour.
func TestOAuthProxySessionIsBounded(t *testing.T) {
	p := openshiftPgAdmin()
	c := oauthProxyContainer(p, testInputs(p, true))
	var expire []string
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--cookie-expire="); ok {
			expire = append(expire, v)
		}
		if strings.HasPrefix(a, "--cookie-refresh=") {
			t.Errorf("%s: a refresh revalidates the token, not the review; it must not stand in for the bound", a)
		}
	}
	if len(expire) != 1 {
		t.Fatalf("--cookie-expire = %v, want exactly one", expire)
	}
	d, err := time.ParseDuration(expire[0])
	if err != nil || d <= 0 || d > time.Hour {
		t.Fatalf("--cookie-expire=%s (%v), want a positive duration of at most 1h", expire[0], err)
	}
}
