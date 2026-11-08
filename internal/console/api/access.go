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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Access requests: the console
// is a UI over PgAccessRequest objects. It reads them with its own
// ServiceAccount (the informer cache) and shows each caller what the API
// server lets them see; every write — a new request, the RoleBinding of an
// approval, the decision in status — goes to the API server with the
// caller's own token. Every decision is the API server's: a
// SelfSubjectAccessReview before anything is read or attempted (403 for
// anything the caller may not do), then RBAC, the RoleBinding escalation
// check and the CRD's CEL rules on the write itself. The one rule the
// console adds is four eyes: nobody approves a request that names them.

// Operation IDs of the access-request endpoints (also fixture names).
const (
	OpListAccessRequests   = "listAccessRequests"
	OpGetAccessRequest     = "getAccessRequest"
	OpCreateAccessRequest  = "createAccessRequest"
	OpApproveAccessRequest = "approveAccessRequest"
	OpDenyAccessRequest    = "denyAccessRequest"
	OpRevokeAccessRequest  = "revokeAccessRequest"

	accessResource = "pgaccessrequests"
	rbacGroup      = "rbac.authorization.k8s.io"
)

// AccessStore is the console ServiceAccount's view of the requests: the
// informer cache over the watched namespaces.
type AccessStore interface {
	// Namespaces are the console's watched namespaces.
	Namespaces() []string
	// Readable reports whether the console can read requests in
	// namespace (the CRD is served and its Role grants the read).
	Readable(namespace string) bool
	// Served is false only when the API server answered that it does not
	// serve PgAccessRequest: a team install without the operator. No answer yet, or only Forbidden ones, counts as served, so
	// the API server keeps deciding.
	Served() bool
	// AccessRequests returns the namespace's requests from the cache.
	AccessRequests(ctx context.Context, namespace string) ([]pgtoolboxv1alpha1.PgAccessRequest, error)
}

// AccessWriter performs the writes as the caller (ops.AccessWriter).
type AccessWriter interface {
	ReadAccessRequest(ctx context.Context, namespace, name string) (*pgtoolboxv1alpha1.PgAccessRequest, error)
	CreateAccessRequest(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) (*pgtoolboxv1alpha1.PgAccessRequest, error)
	CreateGrant(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error
	RecordDecision(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error
}

// Access is what the access-request endpoints need.
type Access struct {
	Store AccessStore
	// Writer is nil on a console that serves no writes
	// (OPERATIONS_ENABLED=false): requests are listed, not made or decided.
	Writer AccessWriter
	// Now is injectable for the fixtures and tests.
	Now func() time.Time
	// Audit receives one line per write attempt; nil discards it.
	Audit *slog.Logger
}

// errNoOperator answers a write where PgAccessRequest is not served: no
// role the caller could ask for would make it work.
func errNoOperator() error {
	return huma.Error501NotImplemented("access requests need the pgtoolbox operator")
}

// writable reports whether requests can be made and decided here: the
// console serves writes and the API server serves the kind.
func (a *Access) writable() bool { return a.Writer != nil && a.Store.Served() }

// refuseWrite is the 501 of a write this console cannot serve, nil when
// it can: the kind not served first (nothing would make it work), then
// writes not enabled.
func (a *Access) refuseWrite() error {
	switch {
	case !a.Store.Served():
		return errNoOperator()
	case a.Writer == nil:
		return huma.Error501NotImplemented("this console serves no writes (OPERATIONS_ENABLED=false)")
	}
	return nil
}

func (a *Access) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// AccessPath identifies one request.
type AccessPath struct {
	Namespace string `path:"namespace" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Namespace of the request (one of the console's watched namespaces)."`
	Name      string `path:"name" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Name of the PgAccessRequest."`
}

type (
	accessListInput struct {
		Namespace string `query:"namespace" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Only this watched namespace."`
	}
	accessCreateInput struct {
		Namespace string `path:"namespace" maxLength:"63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" doc:"Namespace to request access in (one of the console's watched namespaces)."`
		CSRFHeader
		Body viewmodel.AccessRequestCreate
	}
	accessDecisionInput struct {
		AccessPath
		CSRFHeader
		Body viewmodel.AccessDecision
	}
	accessListOutput    struct{ Body viewmodel.AccessRequestList }
	accessRequestOutput struct{ Body viewmodel.AccessRequest }
	accessCreatedOutput struct {
		Status int
		Body   viewmodel.AccessRequest
	}
)

// decisionKind is one of the three decisions.
type decisionKind string

const (
	decideApprove decisionKind = "approve"
	decideDeny    decisionKind = "deny"
	decideRevoke  decisionKind = "revoke"
)

var accessErrors = []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotImplemented, http.StatusServiceUnavailable}

// registerAccess adds the access-request operations.
func registerAccess(a huma.API, d Deps) {
	base := "/api/" + Version
	nsPath := base + "/namespaces/{namespace}/access-requests"
	huma.Register(a, huma.Operation{
		OperationID: OpListAccessRequests, Method: http.MethodGet, Path: base + "/access-requests",
		Summary: "Access requests you may see", Tags: []string{"access"},
		Description: "Per watched namespace: every PgAccessRequest when the API server lets you list them, otherwise the ones naming you " +
			"(your user, your ServiceAccount, a group you are in); with what you may do (request, approve or deny, revoke), each decided " +
			"by a SelfSubjectAccessReview. An approval binds pgtoolbox-console-viewer or pgtoolbox-console-operator for every Cluster of the namespace. " +
			"Refetch on the stream's clusters invalidation.",
		Errors: accessErrors,
	}, func(ctx context.Context, in *accessListInput) (*accessListOutput, error) {
		p, err := accessPrincipal(ctx, d)
		if err != nil {
			return nil, err
		}
		v, err := listAccess(ctx, d, p, in.Namespace)
		if err != nil {
			return nil, err
		}
		return &accessListOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpGetAccessRequest, Method: http.MethodGet, Path: nsPath + "/{name}",
		Summary: "One access request", Tags: []string{"access"},
		Description: "Shown when the API server lets you get it or when it names you; 403 otherwise, whether it exists or not.",
		Errors:      append([]int{http.StatusNotFound}, accessErrors...),
	}, func(ctx context.Context, in *AccessPath) (*accessRequestOutput, error) {
		p, err := accessPrincipal(ctx, d)
		if err != nil {
			return nil, err
		}
		v, err := getAccess(ctx, d, p, in)
		if err != nil {
			return nil, err
		}
		return &accessRequestOutput{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpCreateAccessRequest, Method: http.MethodPost, Path: nsPath,
		Summary: "Request access", Tags: []string{"access"},
		Description: "Creates a PgAccessRequest with your own token (you need create pgaccessrequests in the namespace; pgtoolbox-console-viewer " +
			"grants it). The subject defaults to you. 422: the request is malformed (an expiry in the past, a system: subject, a " +
			"ServiceAccount of another namespace). Requires the X-PgToolBox-Request: 1 header and a same-origin request.",
		DefaultStatus: http.StatusCreated,
		Errors:        append([]int{http.StatusNotFound, http.StatusUnprocessableEntity}, accessErrors...),
	}, func(ctx context.Context, in *accessCreateInput) (*accessCreatedOutput, error) {
		p, err := accessPrincipal(ctx, d)
		if err != nil {
			return nil, err
		}
		v, err := createAccess(ctx, d, p, in.Namespace, in.Body)
		if err != nil {
			return nil, err
		}
		return &accessCreatedOutput{Status: http.StatusCreated, Body: v}, nil
	})

	decisions := []struct {
		id, summary, desc string
		kind              decisionKind
	}{
		{OpApproveAccessRequest, "Approve an access request", "Pessimistic, as you: the console creates the RoleBinding the request resolves to " +
			"(the API server refuses it unless you hold every rule of the ClusterRole: its escalation check), then records you in " +
			"status.decidedBy with an update locked to the version it read. You need create rolebindings and update " +
			"pgaccessrequests/status in the namespace (pgtoolbox-console-operator grants both). 403: you may not, or the request names you " +
			"(nobody approves their own request). 409: the request is no longer Pending, has expired, or changed since it was read.", decideApprove},
		{OpDenyAccessRequest, "Deny an access request", "Records the refusal and your note (required) in status, as you. Needs the same " +
			"rights as approving. 409: the request is no longer Pending. 422: no note.", decideDeny},
		{OpRevokeAccessRequest, "Revoke an approved request", "Records the revocation in status, as you (update pgaccessrequests/status); " +
			"the operator then deletes the RoleBinding, which the request's condition reports. 409: the request is not Approved.", decideRevoke},
	}
	for _, op := range decisions {
		kind := op.kind
		huma.Register(a, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: nsPath + "/{name}/" + string(kind),
			Summary: op.summary, Description: op.desc + " Requires the X-PgToolBox-Request: 1 header and a same-origin request.",
			Tags:   []string{"access"},
			Errors: append([]int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity}, accessErrors...),
		}, func(ctx context.Context, in *accessDecisionInput) (*accessRequestOutput, error) {
			p, err := accessPrincipal(ctx, d)
			if err != nil {
				return nil, err
			}
			v, err := decideAccess(ctx, d, p, &in.AccessPath, kind, in.Body)
			if err != nil {
				return nil, err
			}
			return &accessRequestOutput{Body: v}, nil
		})
	}
}

func accessPrincipal(ctx context.Context, d Deps) (*auth.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if d.Access == nil || d.Access.Store == nil {
		return nil, huma.Error501NotImplemented("access requests are not served by this console")
	}
	return p, nil
}

// accessRights are the caller's answers in one namespace.
type accessRights struct {
	seesAll, canRequest, canReview, canRevoke authz.Decision
}

func askAll(ctx context.Context, az Authorizer, u authz.User, attrs ...authz.Attributes) authz.Decision {
	for _, at := range attrs {
		if d := az.Allowed(ctx, u, at); !d.Allowed {
			return d
		}
	}
	return authz.Decision{Allowed: true}
}

func listAttr(ns string) authz.Attributes {
	return authz.Attributes{Namespace: ns, Verb: "list", Group: authz.PgToolBoxGroup, Resource: accessResource}
}

func createAttr(ns string) authz.Attributes {
	return authz.Attributes{Namespace: ns, Verb: "create", Group: authz.PgToolBoxGroup, Resource: accessResource}
}

func statusAttr(ns, name string) authz.Attributes {
	return authz.Attributes{Namespace: ns, Verb: "update", Group: authz.PgToolBoxGroup, Resource: accessResource, Subresource: "status", Name: name}
}

func bindAttr(ns string) authz.Attributes {
	return authz.Attributes{Namespace: ns, Verb: "create", Group: rbacGroup, Resource: "rolebindings"}
}

func rightsIn(ctx context.Context, az Authorizer, u authz.User, ns string) accessRights {
	return accessRights{
		seesAll:    az.Allowed(ctx, u, listAttr(ns)),
		canRequest: az.Allowed(ctx, u, createAttr(ns)),
		canReview:  askAll(ctx, az, u, bindAttr(ns), statusAttr(ns, "")),
		canRevoke:  az.Allowed(ctx, u, statusAttr(ns, "")),
	}
}

func listAccess(ctx context.Context, d Deps, p *auth.Principal, only string) (viewmodel.AccessRequestList, error) {
	a := d.Access
	now := a.now()
	// Writable keeps its meaning (the console serves writes); where the
	// kind is not served nobody can request or review, whatever RBAC says.
	out := viewmodel.AccessRequestList{GeneratedAt: now, Namespaces: []viewmodel.AccessNamespace{}, Items: []viewmodel.AccessRequest{}, Writable: a.Writer != nil}
	writable := a.writable()
	for _, ns := range a.Store.Namespaces() {
		if only != "" && ns != only {
			continue
		}
		rights := rightsIn(ctx, d.Authz, p.User, ns)
		if rights.seesAll.Err != nil {
			out.Unchecked++
		}
		requests, err := a.Store.AccessRequests(ctx, ns)
		if err != nil {
			return viewmodel.AccessRequestList{}, huma.Error503ServiceUnavailable("the access requests are not available yet")
		}
		shown := 0
		for i := range requests {
			r := &requests[i]
			if !rights.seesAll.Allowed && !r.NamesUser(p.User.Name, p.User.Groups) {
				continue
			}
			out.Items = append(out.Items, accessView(r, p.User, rights, writable, now))
			shown++
		}
		if shown == 0 && !rights.seesAll.Allowed && !rights.canRequest.Allowed && !rights.canReview.Allowed {
			// Nothing to see or do here: the namespace is not named.
			continue
		}
		out.Namespaces = append(out.Namespaces, viewmodel.AccessNamespace{
			Name: ns, SeesAll: rights.seesAll.Allowed, CanRequest: rights.canRequest.Allowed && writable,
			CanReview: rights.canReview.Allowed && writable, Readable: a.Store.Readable(ns),
		})
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		if !out.Items[i].CreatedAt.Equal(out.Items[j].CreatedAt) {
			return out.Items[i].CreatedAt.After(out.Items[j].CreatedAt)
		}
		return out.Items[i].Namespace+"/"+out.Items[i].Name < out.Items[j].Namespace+"/"+out.Items[j].Name
	})
	return out, nil
}

// watched reports whether ns is one of the console's namespaces.
func (a *Access) watched(ns string) bool { return slices.Contains(a.Store.Namespaces(), ns) }

func getAccess(ctx context.Context, d Deps, p *auth.Principal, in *AccessPath) (viewmodel.AccessRequest, error) {
	a := d.Access
	dec := d.Authz.Allowed(ctx, p.User, authz.Attributes{Namespace: in.Namespace, Verb: "get", Group: authz.PgToolBoxGroup, Resource: accessResource, Name: in.Name})
	if dec.Err != nil {
		return viewmodel.AccessRequest{}, huma.Error503ServiceUnavailable("the Kubernetes API server could not be asked whether you may see this request; retry")
	}
	var found *pgtoolboxv1alpha1.PgAccessRequest
	if a.watched(in.Namespace) {
		requests, err := a.Store.AccessRequests(ctx, in.Namespace)
		if err != nil {
			return viewmodel.AccessRequest{}, huma.Error503ServiceUnavailable("the access requests are not available yet")
		}
		for i := range requests {
			if requests[i].Name == in.Name {
				found = &requests[i]
			}
		}
	}
	mine := found != nil && found.NamesUser(p.User.Name, p.User.Groups)
	switch {
	case !dec.Allowed && !mine:
		return viewmodel.AccessRequest{}, huma.Error403Forbidden("you may not get pgaccessrequests/" + in.Name + " in namespace " + in.Namespace)
	case found == nil:
		return viewmodel.AccessRequest{}, huma.Error404NotFound("no such access request in the console's watched namespaces")
	}
	return accessView(found, p.User, rightsIn(ctx, d.Authz, p.User, in.Namespace), a.writable(), a.now()), nil
}

// denied maps a refused or undecided review onto the problem it is.
func denied(dec authz.Decision, what string) error {
	if dec.Err != nil {
		return huma.Error503ServiceUnavailable("the Kubernetes API server could not be asked whether you may " + what + "; retry")
	}
	return huma.Error403Forbidden("you may not " + what)
}

func createAccess(ctx context.Context, d Deps, p *auth.Principal, ns string, body viewmodel.AccessRequestCreate) (viewmodel.AccessRequest, error) {
	a := d.Access
	if err := a.refuseWrite(); err != nil {
		return viewmodel.AccessRequest{}, err
	}
	if dec := d.Authz.Allowed(ctx, p.User, createAttr(ns)); !dec.Allowed {
		return viewmodel.AccessRequest{}, denied(dec, "create pgaccessrequests in namespace "+ns+": ask for the pgtoolbox-console-viewer role there")
	}
	if !a.watched(ns) {
		return viewmodel.AccessRequest{}, huma.Error404NotFound("namespace " + ns + " is not watched by this console")
	}
	now := a.now()
	r, err := NewAccessRequest(ns, p.User.Name, body, now)
	if err != nil {
		a.audit(p, "request", ns, "", err)
		return viewmodel.AccessRequest{}, huma.Error422UnprocessableEntity(err.Error())
	}
	// The write carries the caller's token: the user client refuses a
	// context without one.
	created, err := a.Writer.CreateAccessRequest(p.Context(ctx), r)
	a.audit(p, "request", ns, nameOf(created), err)
	if err != nil {
		return viewmodel.AccessRequest{}, operationError(err)
	}
	return accessView(created, p.User, rightsIn(ctx, d.Authz, p.User, ns), true, now), nil
}

func nameOf(r *pgtoolboxv1alpha1.PgAccessRequest) string {
	if r == nil {
		return ""
	}
	return r.Name
}

// NewAccessRequest builds the PgAccessRequest of a create body: the
// subject defaults to the caller, the request's own rules are checked
// before the API server's (the same ones its CEL holds, so the caller
// gets a readable 422), and the name is generated by the API server.
func NewAccessRequest(ns, username string, body viewmodel.AccessRequestCreate, now time.Time) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	capability := pgtoolboxv1alpha1.AccessCapability(body.Capability)
	if pgtoolboxv1alpha1.ClusterRoleFor(capability) == "" {
		return nil, fmt.Errorf("capability must be viewer or operator, not %q", body.Capability)
	}
	justification := strings.TrimSpace(body.Justification)
	if justification == "" || len(justification) > 1024 {
		return nil, errors.New("a justification of 1 to 1024 characters is required")
	}
	var subject pgtoolboxv1alpha1.AccessSubject
	if body.Subject == nil {
		s, ok := pgtoolboxv1alpha1.SubjectForUsername(username, ns)
		if !ok {
			return nil, fmt.Errorf("you are signed in as %q, which a request cannot name; name the subject explicitly", username)
		}
		subject = s
	} else {
		subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectKind(body.Subject.Kind), Name: body.Subject.Name}
	}
	// A defaulted subject is checked too: the username is the API
	// server's, but the request must still be one the CRD accepts.
	if err := checkSubject(subject); err != nil {
		return nil, err
	}
	r := &pgtoolboxv1alpha1.PgAccessRequest{
		TypeMeta:   metav1.TypeMeta{APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "PgAccessRequest"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, GenerateName: body.Capability + "-"},
		Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
			Subject: subject, Cluster: body.Cluster, Capability: capability, Justification: justification,
		},
	}
	if body.ExpiresAt != nil {
		// The API server stores seconds: the check is on what is stored.
		expires := body.ExpiresAt.UTC().Truncate(time.Second)
		if !expires.After(now) {
			return nil, errors.New("expiresAt must be in the future")
		}
		r.Spec.ExpiresAt = &metav1.Time{Time: expires}
	}
	return r, nil
}

// checkSubject holds the CRD's subject rules, so a mistake is a 422 with
// words rather than the API server's CEL message.
func checkSubject(s pgtoolboxv1alpha1.AccessSubject) error {
	switch {
	case s.Name == "" || len(s.Name) > 512:
		return errors.New("the subject's name must be 1 to 512 characters")
	case strings.ContainsFunc(s.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return errors.New("the subject's name contains a control character")
	}
	switch s.Kind {
	case pgtoolboxv1alpha1.AccessSubjectUser, pgtoolboxv1alpha1.AccessSubjectGroup:
		if strings.HasPrefix(s.Name, "system:") {
			return errors.New("system: users and groups cannot be granted access through a request; a ServiceAccount is named by kind ServiceAccount and its name")
		}
	case pgtoolboxv1alpha1.AccessSubjectServiceAccount:
		if len(s.Name) > 253 || !dnsSubdomain(s.Name) {
			return errors.New("a ServiceAccount subject is the name of a ServiceAccount in this namespace")
		}
	default:
		return fmt.Errorf("subject kind must be User, Group or ServiceAccount, not %q", s.Kind)
	}
	return nil
}

func dnsSubdomain(s string) bool {
	if s == "" || s[0] == '-' || s[0] == '.' || s[len(s)-1] == '-' || s[len(s)-1] == '.' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func decideAccess(ctx context.Context, d Deps, p *auth.Principal, in *AccessPath, kind decisionKind, body viewmodel.AccessDecision) (viewmodel.AccessRequest, error) {
	a := d.Access
	if err := a.refuseWrite(); err != nil {
		return viewmodel.AccessRequest{}, err
	}
	var dec authz.Decision
	if kind == decideRevoke {
		dec = d.Authz.Allowed(ctx, p.User, statusAttr(in.Namespace, in.Name))
	} else {
		dec = askAll(ctx, d.Authz, p.User, bindAttr(in.Namespace), statusAttr(in.Namespace, in.Name))
	}
	if !dec.Allowed {
		what := "create rolebindings and update pgaccessrequests/status in namespace " + in.Namespace + ": deciding access requests takes the pgtoolbox-console-operator role there"
		if kind == decideRevoke {
			what = "update pgaccessrequests/" + in.Name + "/status in namespace " + in.Namespace
		}
		return viewmodel.AccessRequest{}, denied(dec, what)
	}
	if !a.watched(in.Namespace) {
		return viewmodel.AccessRequest{}, huma.Error404NotFound("namespace " + in.Namespace + " is not watched by this console")
	}
	// The read and the writes carry the caller's token: the user client
	// refuses a context without one.
	userCtx := p.Context(ctx)
	r, err := a.Writer.ReadAccessRequest(userCtx, in.Namespace, in.Name)
	if err != nil {
		return viewmodel.AccessRequest{}, operationError(err)
	}
	now := a.now()
	err = applyDecision(userCtx, a.Writer, r, p.User, kind, strings.TrimSpace(body.Note), now)
	a.audit(p, string(kind), in.Namespace, in.Name, err)
	if err != nil {
		return viewmodel.AccessRequest{}, err
	}
	return accessView(r, p.User, rightsIn(ctx, d.Authz, p.User, in.Namespace), true, now), nil
}

// maxDecisionRetries bounds the re-reads after a status write refused for
// a stale version: the operator writes the same status (Pending, its
// conditions) and can win the race with a decision made a moment after the
// request was created.
const maxDecisionRetries = 3

// applyDecision checks the request's state against the decision and
// writes it as the caller: for an approval the RoleBinding first, then the
// status (a status write that fails leaves a binding the operator removes
// after its grace; a status written first would claim a grant that may
// never exist). The status write is locked to the version read; when it is
// refused as stale, the request is read again and the decision re-judged
// on that read — the same request (UID and spec), still in a phase the
// decision accepts, four eyes again — before it is written again. A
// decision someone else recorded in between is therefore a 409, never
// overwritten. On success r holds what was written.
func applyDecision(ctx context.Context, w AccessWriter, r *pgtoolboxv1alpha1.PgAccessRequest, caller authz.User, kind decisionKind, note string, now time.Time) error {
	if why := decisionRefusal(r, caller, kind, note, now); why != nil {
		return why
	}
	if kind == decideApprove {
		if err := w.CreateGrant(ctx, r); err != nil {
			return operationError(err)
		}
	}
	judged := r.DeepCopy()
	for attempt := 0; ; attempt++ {
		recordDecision(r, caller, kind, note, now)
		err := w.RecordDecision(ctx, r)
		if err == nil {
			return nil
		}
		var status interface{ HTTPStatus() int }
		if attempt >= maxDecisionRetries || !errors.As(err, &status) || status.HTTPStatus() != http.StatusConflict {
			return operationError(err)
		}
		fresh, readErr := w.ReadAccessRequest(ctx, judged.Namespace, judged.Name)
		if readErr != nil {
			return operationError(readErr)
		}
		if fresh.UID != judged.UID || !equality.Semantic.DeepEqual(fresh.Spec, judged.Spec) {
			return huma.Error409Conflict("the request was replaced while the decision was being recorded")
		}
		if why := decisionRefusal(fresh, caller, kind, note, now); why != nil {
			return why
		}
		*r = *fresh
	}
}

// recordDecision sets the decision's status fields on r.
func recordDecision(r *pgtoolboxv1alpha1.PgAccessRequest, caller authz.User, kind decisionKind, note string, now time.Time) {
	at := &metav1.Time{Time: now.Truncate(time.Second)}
	switch kind {
	case decideApprove:
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseApproved
		r.Status.DecidedBy, r.Status.DecidedAt = caller.Name, at
		r.Status.RoleBinding, r.Status.ClusterRole = r.RoleBindingName(), pgtoolboxv1alpha1.ClusterRoleFor(r.Spec.Capability)
		r.Status.GrantedScope = pgtoolboxv1alpha1.AccessGrantScopeNamespace
	case decideDeny:
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseDenied
		r.Status.DecidedBy, r.Status.DecidedAt = caller.Name, at
	case decideRevoke:
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseRevoked
		r.Status.RevokedBy, r.Status.RevokedAt = caller.Name, at
	}
	if note != "" {
		r.Status.Note = note
	}
}

// decisionRefusal is the console's own check of a decision against the
// request's state, before anything is written; nil when it may proceed.
func decisionRefusal(r *pgtoolboxv1alpha1.PgAccessRequest, caller authz.User, kind decisionKind, note string, now time.Time) error {
	phase := r.Phase()
	switch kind {
	case decideApprove, decideDeny:
		if phase != pgtoolboxv1alpha1.AccessPhasePending {
			return huma.Error409Conflict("the request is " + string(phase) + ", not Pending")
		}
		if kind == decideDeny && note == "" {
			return huma.Error422UnprocessableEntity("say why the request is denied: the note is shown to the requester")
		}
		if kind == decideApprove && r.ExpiredAt(now) {
			return huma.Error409Conflict("the request expired at " + r.Spec.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if kind == decideApprove && r.NamesUser(caller.Name, caller.Groups) {
			return huma.Error403Forbidden("you may not approve a request that names you: ask someone else to decide")
		}
	case decideRevoke:
		if phase != pgtoolboxv1alpha1.AccessPhaseApproved {
			return huma.Error409Conflict("the request is " + string(phase) + ", not Approved")
		}
	}
	if len(note) > 1024 {
		return huma.Error422UnprocessableEntity("the note is longer than 1024 characters")
	}
	return nil
}

// accessView is one request as the caller sees it, with the decisions
// they may make now.
func accessView(r *pgtoolboxv1alpha1.PgAccessRequest, caller authz.User, rights accessRights, writable bool, now time.Time) viewmodel.AccessRequest {
	v := viewmodel.AccessRequest{
		Namespace: r.Namespace, Name: r.Name,
		Subject:    viewmodel.AccessSubject{Kind: string(r.Spec.Subject.Kind), Name: r.Spec.Subject.Name},
		Capability: string(r.Spec.Capability), ClusterRole: pgtoolboxv1alpha1.ClusterRoleFor(r.Spec.Capability),
		Cluster: r.Spec.Cluster, Justification: r.Spec.Justification, CreatedAt: r.CreationTimestamp.UTC(),
		ExpiresAt: timeOf(r.Spec.ExpiresAt), Phase: string(r.Phase()),
		DecidedBy: r.Status.DecidedBy, DecidedAt: timeOf(r.Status.DecidedAt),
		RevokedBy: r.Status.RevokedBy, RevokedAt: timeOf(r.Status.RevokedAt),
		Note: r.Status.Note, RoleBinding: r.Status.RoleBinding,
		Mine:  r.NamesUser(caller.Name, caller.Groups),
		Scope: "every Cluster of namespace " + r.Namespace + ": a RoleBinding cannot be narrowed to one Cluster",
	}
	if r.Spec.Cluster != "" {
		v.Scope = "every Cluster of namespace " + r.Namespace + ", not only " + r.Spec.Cluster + ": a RoleBinding cannot be narrowed to one Cluster"
	}
	v.Condition = viewmodel.AccessCondition{
		Status: string(metav1.ConditionUnknown), Reason: pgtoolboxv1alpha1.ReasonInitializing.String(),
		Message: "the operator has not reported on this request yet",
	}
	for _, c := range r.Status.Conditions {
		if c.Type == pgtoolboxv1alpha1.ConditionReady {
			v.Condition = viewmodel.AccessCondition{Status: string(c.Status), Reason: c.Reason, Message: c.Message, Observed: c.ObservedGeneration == r.Generation}
		}
	}
	granted := false
	for _, c := range r.Status.Conditions {
		if c.Type == pgtoolboxv1alpha1.ConditionRoleBindingSynced && c.Reason == pgtoolboxv1alpha1.ReasonAccessGranted.String() {
			granted = true
		}
	}
	v.Health = viewmodel.AccessHealth(v.Phase, v.Condition, granted)
	v.Actions = accessActions(r, caller, rights, writable, now)
	return v
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// accessActions decides which of the three decisions the caller may make
// now: the API server's answer, the request's state and four eyes.
func accessActions(r *pgtoolboxv1alpha1.PgAccessRequest, caller authz.User, rights accessRights, writable bool, now time.Time) viewmodel.AccessActions {
	review := func(dec authz.Decision, kind decisionKind) viewmodel.AccessAction {
		switch {
		case !writable:
			return viewmodel.AccessAction{Why: "this console serves no writes (OPERATIONS_ENABLED=false)"}
		case dec.Err != nil:
			return viewmodel.AccessAction{Undecided: true, Why: "the Kubernetes API server could not be asked; retry"}
		case !dec.Allowed && kind == decideRevoke:
			return viewmodel.AccessAction{Why: "you may not update pgaccessrequests/status in namespace " + r.Namespace}
		case !dec.Allowed:
			return viewmodel.AccessAction{Why: "deciding takes create rolebindings and update pgaccessrequests/status in namespace " + r.Namespace}
		}
		// The note is the dialog's to collect: it does not decide whether
		// the action is offered.
		if refusal := decisionRefusal(r, caller, kind, "-", now); refusal != nil {
			return viewmodel.AccessAction{Why: errorDetail(refusal)}
		}
		return viewmodel.AccessAction{Allowed: true}
	}
	return viewmodel.AccessActions{
		Approve: review(rights.canReview, decideApprove),
		Deny:    review(rights.canReview, decideDeny),
		Revoke:  review(rights.canRevoke, decideRevoke),
	}
}

// errorDetail is the detail of a Huma problem.
func errorDetail(err error) string {
	var model *huma.ErrorModel
	if errors.As(err, &model) {
		return model.Detail
	}
	return err.Error()
}

// audit writes the one server audit line per write attempt: who (as the
// API server named them), what, where, and the outcome category.
func (a *Access) audit(p *auth.Principal, action, ns, name string, err error) {
	if a.Audit == nil {
		return
	}
	outcome := "accepted"
	var status interface{ GetStatus() int }
	var httpStatus interface{ HTTPStatus() int }
	switch {
	case err == nil:
	case errors.As(err, &status):
		outcome = "refused " + http.StatusText(status.GetStatus())
	case errors.As(err, &httpStatus):
		outcome = "refused " + http.StatusText(httpStatus.HTTPStatus())
	default:
		outcome = "refused"
	}
	a.Audit.Info("access request",
		slog.String("user", p.User.Name), slog.String("session", p.SessionID), slog.String("action", action),
		slog.String("namespace", ns), slog.String("name", name), slog.String("outcome", outcome))
}
