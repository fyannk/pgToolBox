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

package v1alpha1

import (
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CataloguePgAccessRequest is the owner name of the PgAccessRequest reasons
// in Catalogue.
const CataloguePgAccessRequest = "pgaccessrequest"

// pgAccessRequestReasons are the PgAccessRequest-specific reasons of the
// frozen catalogue.
var pgAccessRequestReasons = []Reason{
	ReasonAwaitingDecision,
	ReasonAccessGranted,
	ReasonAccessDenied,
	ReasonAccessExpired,
	ReasonAccessRevoked,
	ReasonRoleBindingMissing,
	ReasonRoleBindingDrifted,
	ReasonRoleBindingOrphaned,
	ReasonSelfApprovalRefused,
	ReasonRoleBindingDeleted,
}

func init() {
	SchemeBuilder.Register(&PgAccessRequest{}, &PgAccessRequestList{})
	registerReasons(CataloguePgAccessRequest, pgAccessRequestReasons...)
}

// Names the access-request machinery shares between the console (which
// creates the RoleBinding as the approver) and the operator (which deletes
// it): one builder, so the two can never disagree on what a grant is.
const (
	// LabelAccessRequest carries the owning PgAccessRequest's name on its
	// RoleBinding.
	LabelAccessRequest = "pgtoolbox.fyannk.dev/access-request"
	// ClusterRoleViewer and ClusterRoleOperator are the console's two
	// ClusterRoles (config/rbac/pgtoolbox_console_*_clusterrole.yaml): fixed names,
	// because people are bound to them by name.
	ClusterRoleViewer   = "pgtoolbox-console-viewer"
	ClusterRoleOperator = "pgtoolbox-console-operator"
	// AccessRoleBindingPrefix prefixes the RoleBinding of a request.
	AccessRoleBindingPrefix = "pgaccess-"
	// OperatorActor is the revokedBy of a grant the operator revoked.
	OperatorActor = "pgtoolbox-operator"
	// serviceAccountUserPrefix is how the API server names a
	// ServiceAccount: system:serviceaccount:<namespace>:<name>.
	serviceAccountUserPrefix = "system:serviceaccount:"
)

// ClusterRoleFor returns the ClusterRole a capability binds, "" for an
// unknown capability.
func ClusterRoleFor(capability AccessCapability) string {
	switch capability {
	case AccessCapabilityViewer:
		return ClusterRoleViewer
	case AccessCapabilityOperator:
		return ClusterRoleOperator
	}
	return ""
}

// RoleBindingName is the name of the request's RoleBinding.
func (r *PgAccessRequest) RoleBindingName() string { return AccessRoleBindingPrefix + r.Name }

// Phase returns the request's phase, Pending while none is recorded.
func (r *PgAccessRequest) Phase() PgAccessRequestPhase {
	if r.Status.Phase == "" {
		return AccessPhasePending
	}
	return r.Status.Phase
}

// IsTerminal reports a phase no decision can leave.
func (p PgAccessRequestPhase) IsTerminal() bool {
	return p == AccessPhaseDenied || p == AccessPhaseExpired || p == AccessPhaseRevoked
}

// ExpiredAt reports whether spec.expiresAt is set and not after now.
func (r *PgAccessRequest) ExpiredAt(now time.Time) bool {
	return r.Spec.ExpiresAt != nil && !now.Before(r.Spec.ExpiresAt.Time)
}

// RBACSubject is the subject the request's RoleBinding binds.
func (r *PgAccessRequest) RBACSubject() rbacv1.Subject {
	switch r.Spec.Subject.Kind {
	case AccessSubjectServiceAccount:
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: r.Spec.Subject.Name, Namespace: r.Namespace}
	case AccessSubjectGroup:
		return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: r.Spec.Subject.Name}
	default:
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: r.Spec.Subject.Name}
	}
}

// SubjectUsername is the username the API server gives the subject: the
// name of a User, system:serviceaccount:<namespace>:<name> of a
// ServiceAccount, "" for a Group (whose members have their own names).
func (r *PgAccessRequest) SubjectUsername() string {
	switch r.Spec.Subject.Kind {
	case AccessSubjectUser:
		return r.Spec.Subject.Name
	case AccessSubjectServiceAccount:
		return serviceAccountUserPrefix + r.Namespace + ":" + r.Spec.Subject.Name
	case AccessSubjectGroup:
		return "" // members carry their own usernames
	}
	return ""
}

// NamesUser reports whether the request is for the user the API server
// names username with groups: the same User, the same ServiceAccount, or
// a Group they belong to.
func (r *PgAccessRequest) NamesUser(username string, groups []string) bool {
	if r.Spec.Subject.Kind == AccessSubjectGroup {
		for _, group := range groups {
			if group == r.Spec.Subject.Name {
				return true
			}
		}
		return false
	}
	return username != "" && r.SubjectUsername() == username
}

// SubjectForUsername returns the subject a person signed in as username
// requests access for in namespace: a ServiceAccount when username is one
// of that namespace's ServiceAccounts, a User otherwise. ok is false for a
// username no request can name: empty, a ServiceAccount of another
// namespace (references stay in the namespace), or another system: user.
func SubjectForUsername(username, namespace string) (AccessSubject, bool) {
	if username == "" {
		return AccessSubject{}, false
	}
	if rest, isSA := strings.CutPrefix(username, serviceAccountUserPrefix); isSA {
		ns, name, found := strings.Cut(rest, ":")
		if !found || ns != namespace || name == "" || strings.Contains(name, ":") {
			return AccessSubject{}, false
		}
		return AccessSubject{Kind: AccessSubjectServiceAccount, Name: name}, true
	}
	if strings.HasPrefix(username, "system:") {
		return AccessSubject{}, false
	}
	return AccessSubject{Kind: AccessSubjectUser, Name: username}, true
}

// DesiredRoleBinding is the RoleBinding an approved request resolves to:
// the request's subject bound to its capability's ClusterRole in the
// request's namespace, controlled by the request (so deleting the request
// deletes the grant), labelled for the operator's filtered cache. The
// owner reference does not block owner deletion, so creating it needs no
// permission on the request's finalizers.
func (r *PgAccessRequest) DesiredRoleBinding() *rbacv1.RoleBinding {
	controller := true
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.RoleBindingName(),
			Namespace: r.Namespace,
			Labels: map[string]string{
				LabelManagedBy:     ManagedByValue,
				LabelReconcile:     ReconcileLabelValue,
				LabelAccessRequest: r.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: GroupVersion.String(), Kind: "PgAccessRequest",
				Name: r.Name, UID: r.UID, Controller: &controller,
			}},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRoleFor(r.Spec.Capability)},
		Subjects: []rbacv1.Subject{r.RBACSubject()},
	}
}

// GrantMatches reports whether binding grants exactly what the request
// asks: the capability's ClusterRole, to the request's subject alone,
// controlled by this request.
func (r *PgAccessRequest) GrantMatches(binding *rbacv1.RoleBinding) bool {
	want := r.DesiredRoleBinding()
	owner := metav1.GetControllerOfNoCopy(binding)
	return owner != nil && owner.UID == r.UID &&
		binding.RoleRef == want.RoleRef &&
		len(binding.Subjects) == 1 && binding.Subjects[0] == want.Subjects[0]
}
