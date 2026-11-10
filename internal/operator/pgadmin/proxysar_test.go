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
