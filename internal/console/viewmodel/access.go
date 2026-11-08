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

package viewmodel

import "time"

// Access requests: the console is a UI over
// PgAccessRequest objects. A request asks for one of the two console
// ClusterRoles in a namespace; deciding it is a person's act, made with
// their own token.

// AccessSubject is who a request is for.
type AccessSubject struct {
	Kind string `json:"kind" enum:"User,Group,ServiceAccount" doc:"User and Group as the API server names them; ServiceAccount of the request's namespace."`
	Name string `json:"name" minLength:"1" maxLength:"512"`
}

// AccessAction is one decision as offered to the caller.
type AccessAction struct {
	// Allowed is true when the API server lets the caller make it
	// (SelfSubjectAccessReview) and the request's state permits it now.
	Allowed bool `json:"allowed"`
	// Undecided is true when the API server could not be asked.
	Undecided bool `json:"undecided,omitempty"`
	// Why is the reason it is not allowed, in words; empty when allowed.
	Why string `json:"why,omitempty"`
}

// AccessActions are the three decisions on one request.
type AccessActions struct {
	Approve AccessAction `json:"approve"`
	Deny    AccessAction `json:"deny"`
	Revoke  AccessAction `json:"revoke"`
}

// AccessCondition is the operator's summary of a request (its Ready
// condition): whether the namespace's RBAC matches the request's phase.
type AccessCondition struct {
	Status  string `json:"status" enum:"True,False,Unknown"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// Observed is false while the operator has not yet looked at the
	// current generation: the phase is then the decider's word alone.
	Observed bool `json:"observed"`
}

// AccessRequest is one request as the console shows it.
type AccessRequest struct {
	Namespace     string        `json:"namespace"`
	Name          string        `json:"name"`
	Subject       AccessSubject `json:"subject"`
	Capability    string        `json:"capability" enum:"viewer,operator"`
	ClusterRole   string        `json:"clusterRole" doc:"The ClusterRole an approval binds: pgtoolbox-console-viewer or pgtoolbox-console-operator."`
	Cluster       string        `json:"cluster,omitempty" doc:"The Cluster the requester named; informative, see scope."`
	Justification string        `json:"justification"`
	CreatedAt     time.Time     `json:"createdAt"`
	ExpiresAt     *time.Time    `json:"expiresAt,omitempty"`
	Phase         string        `json:"phase" enum:"Pending,Approved,Denied,Expired,Revoked"`
	// Health is how the request is drawn (AccessHealth): the SPA maps no
	// phase to a colour of its own.
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"How the request is drawn: Pending progressing, Approved ok (progressing until the operator confirms the grant), Denied/Expired/Revoked inactive, and warning whenever the operator reports the namespace's RBAC does not match (RoleBindingMissing, RoleBindingDrifted, ClusterNotFound, a reconcile error)."`
	// Scope says, in words, how far an approval reaches: a RoleBinding
	// covers every Cluster of the namespace, whatever Cluster names.
	Scope       string     `json:"scope"`
	DecidedBy   string     `json:"decidedBy,omitempty"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	RevokedBy   string     `json:"revokedBy,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	Note        string     `json:"note,omitempty" doc:"The decider's note (required on deny), or the operator's explanation of a revocation."`
	RoleBinding string     `json:"roleBinding,omitempty"`
	// Mine is true when the request names the caller (their user, their
	// ServiceAccount, or a group they are in).
	Mine      bool            `json:"mine"`
	Condition AccessCondition `json:"condition"`
	Actions   AccessActions   `json:"actions"`
}

// AccessNamespace is what the caller may do with requests in one watched
// namespace.
type AccessNamespace struct {
	Name string `json:"name"`
	// SeesAll is true when the caller may list the namespace's requests;
	// otherwise only the requests naming them are shown.
	SeesAll bool `json:"seesAll"`
	// CanRequest: the caller may create a PgAccessRequest here.
	CanRequest bool `json:"canRequest"`
	// CanReview: the caller may create RoleBindings and update request
	// status here (approve, deny, revoke).
	CanReview bool `json:"canReview"`
	// Readable is false when the console cannot read requests here (the
	// CRD is not installed or its Role lacks the grant).
	Readable bool `json:"readable"`
}

// AccessRequestList is the access-request screen.
type AccessRequestList struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	Namespaces  []AccessNamespace `json:"namespaces"`
	// Items are newest first.
	Items []AccessRequest `json:"items"`
	// Writable is false when the console serves no writes
	// (ALLOW_OPERATIONS=false): requests can be read, not made or decided.
	Writable bool `json:"writable"`
	// Unchecked counts namespaces whose list permission could not be
	// checked; only the caller's own requests are shown there.
	Unchecked int `json:"unchecked,omitempty"`
}

// AccessRequestCreate is the body of a new request.
type AccessRequestCreate struct {
	// Subject defaults to the caller: their user, or their ServiceAccount
	// when they signed in with one of this namespace's.
	Subject       *AccessSubject `json:"subject,omitempty"`
	Cluster       string         `json:"cluster,omitempty" maxLength:"253" pattern:"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" doc:"Informative: an approval covers every Cluster of the namespace."`
	Capability    string         `json:"capability" enum:"viewer,operator"`
	Justification string         `json:"justification" minLength:"1" maxLength:"1024"`
	ExpiresAt     *time.Time     `json:"expiresAt,omitempty" doc:"When the access ends; must be in the future. Omitted: until revoked."`
}

// AccessDecision is the body of approve, deny and revoke.
type AccessDecision struct {
	Note string `json:"note,omitempty" maxLength:"1024" doc:"Required to deny; shown to the requester."`
}

// AccessHealth draws a request from its phase and the operator's Ready
// condition: a False condition is a warning whatever the phase (the RBAC
// does not match the request, or the operator failed); otherwise Pending
// is progressing, Approved is ok once the operator reported the grant in
// place (granted: RoleBindingSynced=AccessGranted; progressing before),
// the terminal phases are inactive, and anything else is unknown.
func AccessHealth(phase string, c AccessCondition, granted bool) Health {
	if c.Status == "False" {
		return HealthWarning
	}
	switch phase {
	case "Pending":
		return HealthProgressing
	case "Approved":
		if granted && c.Observed && c.Status == "True" {
			return HealthOK
		}
		return HealthProgressing
	case "Denied", "Expired", "Revoked":
		return HealthInactive
	}
	return HealthUnknown
}
