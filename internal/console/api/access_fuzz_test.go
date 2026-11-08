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

package api

import (
	"encoding/json"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// FuzzNewAccessRequest: whatever body a client sends and whoever they are
// signed in as, an accepted request names a subject the CRD accepts — never
// a system: user or group, never a ServiceAccount outside the namespace —
// with a known capability, a bounded justification and an expiry in the
// future.
func FuzzNewAccessRequest(f *testing.F) {
	for _, seed := range []struct{ body, username string }{
		{`{"capability":"viewer","justification":"read"}`, "oidc:jane"},
		{`{"capability":"operator","justification":"x","subject":{"kind":"Group","name":"system:authenticated"}}`, "oidc:jane"},
		{`{"capability":"operator","justification":"x","subject":{"kind":"ServiceAccount","name":"ci"}}`, "oidc:jane"},
		{`{"capability":"viewer","justification":"x"}`, "system:serviceaccount:team-a:ci"},
		{`{"capability":"viewer","justification":"x"}`, "system:serviceaccount:team-b:ci"},
		{`{"capability":"viewer","justification":"x","expiresAt":"2020-01-01T00:00:00Z"}`, "jane"},
		{`{"capability":"viewer","justification":"x","subject":{"kind":"User","name":"a\u0000"}}`, "jane"},
	} {
		f.Add(seed.body, seed.username)
	}
	now := fixtures.Now
	f.Fuzz(func(t *testing.T, raw, username string) {
		var body viewmodel.AccessRequestCreate
		if json.Unmarshal([]byte(raw), &body) != nil {
			return
		}
		r, err := NewAccessRequest("team-a", username, body, now)
		if err != nil {
			return
		}
		s := r.Spec.Subject
		switch s.Kind {
		case pgtoolboxv1alpha1.AccessSubjectUser, pgtoolboxv1alpha1.AccessSubjectGroup:
			if strings.HasPrefix(s.Name, "system:") {
				t.Fatalf("accepted a system: subject %+v", s)
			}
		case pgtoolboxv1alpha1.AccessSubjectServiceAccount:
			if !dnsSubdomain(s.Name) {
				t.Fatalf("accepted ServiceAccount %q", s.Name)
			}
		default:
			t.Fatalf("accepted subject kind %q", s.Kind)
		}
		if s.Name == "" || len(s.Name) > 512 || strings.ContainsAny(s.Name, "\x00\n\r\t") {
			t.Fatalf("accepted subject name %q", s.Name)
		}
		if pgtoolboxv1alpha1.ClusterRoleFor(r.Spec.Capability) == "" {
			t.Fatalf("accepted capability %q", r.Spec.Capability)
		}
		if r.Spec.Justification == "" || len(r.Spec.Justification) > 1024 {
			t.Fatalf("accepted justification of %d bytes", len(r.Spec.Justification))
		}
		if r.Spec.ExpiresAt != nil && !r.Spec.ExpiresAt.After(now) {
			t.Fatalf("accepted an expiry in the past: %v", r.Spec.ExpiresAt)
		}
		if r.Namespace != "team-a" || r.Name != "" {
			t.Fatalf("request lands in %s/%s", r.Namespace, r.Name)
		}
	})
}
