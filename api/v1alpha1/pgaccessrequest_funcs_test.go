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

package v1alpha1_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func accessRequest(kind pgtoolboxv1alpha1.AccessSubjectKind, name string) *pgtoolboxv1alpha1.PgAccessRequest {
	return &pgtoolboxv1alpha1.PgAccessRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "jane-operator", UID: "8f9e0c1d-0000-4000-8000-000000000001"},
		Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
			Subject: pgtoolboxv1alpha1.AccessSubject{Kind: kind, Name: name}, Capability: pgtoolboxv1alpha1.AccessCapabilityOperator, Justification: "x",
		},
	}
}

// TestDesiredRoleBindingGolden holds the one RoleBinding builder the
// console (create, as the approver) and the operator (drift check) share.
func TestDesiredRoleBindingGolden(t *testing.T) {
	for _, tc := range []struct {
		file string
		req  *pgtoolboxv1alpha1.PgAccessRequest
	}{
		{"rolebinding-user.golden.yaml", accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "oidc:jane@corp.example")},
		{"rolebinding-group.golden.yaml", accessRequest(pgtoolboxv1alpha1.AccessSubjectGroup, "dba")},
		{"rolebinding-serviceaccount.golden.yaml", accessRequest(pgtoolboxv1alpha1.AccessSubjectServiceAccount, "ci")},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got, err := yaml.Marshal(tc.req.DesiredRoleBinding())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "pgaccessrequest", tc.file)
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) // #nosec G304 -- test fixture path
			if err != nil {
				t.Fatalf("%v (go test ./api/v1alpha1 -run TestDesiredRoleBindingGolden -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("RoleBinding drifted from %s:\n%s", path, got)
			}
			if !tc.req.GrantMatches(tc.req.DesiredRoleBinding()) {
				t.Fatal("the desired binding does not match its own request")
			}
		})
	}
}

func TestGrantMatches(t *testing.T) {
	req := accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "jane")
	cases := []struct {
		name   string
		mutate func(*rbacv1.RoleBinding)
		want   bool
	}{
		{"as built", func(*rbacv1.RoleBinding) {}, true},
		{"another ClusterRole", func(b *rbacv1.RoleBinding) { b.RoleRef.Name = "admin" }, false},
		{"a Role of the same name", func(b *rbacv1.RoleBinding) { b.RoleRef.Kind = "Role" }, false},
		{"an extra subject", func(b *rbacv1.RoleBinding) {
			b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "mallory"})
		}, false},
		{"another subject", func(b *rbacv1.RoleBinding) { b.Subjects[0].Name = "mallory" }, false},
		{"no controller", func(b *rbacv1.RoleBinding) { b.OwnerReferences = nil }, false},
		{"another controller", func(b *rbacv1.RoleBinding) { b.OwnerReferences[0].UID = "other" }, false},
	}
	for _, tc := range cases {
		b := req.DesiredRoleBinding()
		tc.mutate(b)
		if got := req.GrantMatches(b); got != tc.want {
			t.Errorf("%s: GrantMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSubjectForUsername(t *testing.T) {
	cases := []struct {
		username, namespace string
		want                pgtoolboxv1alpha1.AccessSubject
		ok                  bool
	}{
		{"oidc:jane@corp.example", "team-a", pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: "oidc:jane@corp.example"}, true},
		{"system:serviceaccount:team-a:ci", "team-a", pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "ci"}, true},
		{"system:serviceaccount:team-b:ci", "team-a", pgtoolboxv1alpha1.AccessSubject{}, false},
		{"system:serviceaccount:team-a:", "team-a", pgtoolboxv1alpha1.AccessSubject{}, false},
		{"system:serviceaccount:team-a:ci:x", "team-a", pgtoolboxv1alpha1.AccessSubject{}, false},
		{"system:admin", "team-a", pgtoolboxv1alpha1.AccessSubject{}, false},
		{"", "team-a", pgtoolboxv1alpha1.AccessSubject{}, false},
	}
	for _, tc := range cases {
		got, ok := pgtoolboxv1alpha1.SubjectForUsername(tc.username, tc.namespace)
		if got != tc.want || ok != tc.ok {
			t.Errorf("SubjectForUsername(%q, %q) = %+v %v, want %+v %v", tc.username, tc.namespace, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNamesUser(t *testing.T) {
	cases := []struct {
		req      *pgtoolboxv1alpha1.PgAccessRequest
		username string
		groups   []string
		want     bool
	}{
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "jane"), "jane", nil, true},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "jane"), "bob", []string{"jane"}, false},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectGroup, "dba"), "bob", []string{"dev", "dba"}, true},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectGroup, "dba"), "dba", nil, false},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectServiceAccount, "ci"), "system:serviceaccount:team-a:ci", nil, true},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectServiceAccount, "ci"), "ci", nil, false},
		{accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "jane"), "", nil, false},
	}
	for _, tc := range cases {
		if got := tc.req.NamesUser(tc.username, tc.groups); got != tc.want {
			t.Errorf("%s %s: NamesUser(%q, %v) = %v, want %v", tc.req.Spec.Subject.Kind, tc.req.Spec.Subject.Name, tc.username, tc.groups, got, tc.want)
		}
	}
}

func TestAccessPhaseHelpers(t *testing.T) {
	r := accessRequest(pgtoolboxv1alpha1.AccessSubjectUser, "jane")
	if r.Phase() != pgtoolboxv1alpha1.AccessPhasePending {
		t.Fatalf("an unrecorded phase is %s, want Pending", r.Phase())
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if r.ExpiredAt(now) {
		t.Fatal("a request without expiresAt expired")
	}
	r.Spec.ExpiresAt = &metav1.Time{Time: now}
	if !r.ExpiredAt(now) || r.ExpiredAt(now.Add(-time.Second)) {
		t.Fatal("ExpiredAt is not 'at or after expiresAt'")
	}
	for phase, terminal := range map[pgtoolboxv1alpha1.PgAccessRequestPhase]bool{"Pending": false, "Approved": false, "Denied": true, "Expired": true, "Revoked": true} {
		if phase.IsTerminal() != terminal {
			t.Errorf("%s.IsTerminal() = %v", phase, !terminal)
		}
	}
	if pgtoolboxv1alpha1.ClusterRoleFor("admin") != "" {
		t.Fatal("an unknown capability maps to a ClusterRole")
	}
}

// FuzzSubjectForUsername: whatever the username, a subject is returned only
// when a request can name it, and the subject names that user back.
func FuzzSubjectForUsername(f *testing.F) {
	for _, seed := range []string{"jane", "oidc:jane@corp.example", "system:serviceaccount:team-a:ci", "system:serviceaccount:team-a:", "system:serviceaccount:x:y:z", "system:masters", ""} {
		f.Add(seed, "team-a")
	}
	f.Fuzz(func(t *testing.T, username, namespace string) {
		subject, ok := pgtoolboxv1alpha1.SubjectForUsername(username, namespace)
		if !ok {
			if subject != (pgtoolboxv1alpha1.AccessSubject{}) {
				t.Fatalf("refused %q but returned %+v", username, subject)
			}
			return
		}
		if subject.Kind == pgtoolboxv1alpha1.AccessSubjectUser && strings.HasPrefix(subject.Name, "system:") {
			t.Fatalf("%q became a system: user subject", username)
		}
		r := &pgtoolboxv1alpha1.PgAccessRequest{ObjectMeta: metav1.ObjectMeta{Namespace: namespace}, Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{Subject: subject}}
		if !r.NamesUser(username, nil) {
			t.Fatalf("the subject %+v of %q does not name them back", subject, username)
		}
	})
}
