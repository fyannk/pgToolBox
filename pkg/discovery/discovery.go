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

// Package discovery probes optional APIs the way the operator and the
// console both need to: a scoped List with
// Limit 1, which answers the only question that matters — "may this
// ServiceAccount list this kind here?" — in one request, without the
// cluster-wide discovery documents or a RESTMapper refresh. Forbidden and
// "not served" turn the feature off with a reason; any other error is
// returned, because a start-up that cannot tell is a start-up that
// should fail rather than guess.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// State is the outcome of one probe.
type State string

// The probe outcomes.
const (
	// Available means the List succeeded.
	Available State = "available"
	// Forbidden means RBAC denied the List to the caller.
	Forbidden State = "forbidden"
	// NotInstalled means the API server does not serve the kind (no CRD,
	// or a CNPG minor without it).
	NotInstalled State = "notInstalled"
)

// Result is one probe outcome with a bounded, secret-free reason.
type Result struct {
	GVK       schema.GroupVersionKind
	Namespace string
	State     State
	// Reason explains Forbidden and NotInstalled in words, naming the
	// resource and namespace; empty when Available.
	Reason string
	// RefusedIn names, for a kind Available in some of the namespaces
	// probed only, the namespaces where it is refused (ProbeKinds), in the
	// order they were probed; empty otherwise. The feature is off in those
	// namespaces alone (In).
	RefusedIn []string
}

// Usable reports whether the feature can be watched somewhere: in every
// namespace probed, except those of RefusedIn. A reconciler of an object
// asks In(its namespace).Usable() instead.
func (r Result) Usable() bool { return r.State == Available }

// In is the outcome in one namespace: Forbidden, with a reason naming the
// namespace, where the kind is refused there while other namespaces allow
// it; the outcome everywhere otherwise.
func (r Result) In(namespace string) Result {
	if r.State != Available || !slices.Contains(r.RefusedIn, namespace) {
		return r
	}
	return Result{
		GVK: r.GVK, Namespace: namespace, State: Forbidden,
		Reason: fmt.Sprintf("not allowed to list %s%s (other watched namespaces allow it)", resourceName(r.GVK), where(namespace)),
	}
}

// ListableIn is the namespaces of watched where the kind can be watched,
// for a cache restricted to them; nil when it can be watched in all of them
// (or the kind is not usable at all).
func (r Result) ListableIn(watched []string) []string {
	if !r.Usable() || len(r.RefusedIn) == 0 {
		return nil
	}
	var listable []string
	for _, namespace := range watched {
		if !slices.Contains(r.RefusedIn, namespace) {
			listable = append(listable, namespace)
		}
	}
	return listable
}

// Lister is the one method a probe needs; a controller-runtime client,
// an APIReader or a fake satisfy it.
type Lister interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// ProbeListable lists at most one object of gvk, as metadata only, in
// namespace ("" for a cluster-scoped kind or a cluster-wide list). It
// returns Available, Forbidden or NotInstalled; every other error is
// returned unclassified so the caller fails its start (or, re-probing,
// keeps what it has), Unauthorized included: a 401 says the caller's
// credentials were not accepted (a token being rotated, an authentication
// webhook down), not that RBAC refuses the kind, and turning the feature
// off would last for the life of the process.
func ProbeListable(ctx context.Context, c Lister, gvk schema.GroupVersionKind, namespace string) (Result, error) {
	return classify(gvk, namespace, listOne(ctx, c, gvk, namespace))
}

// listOne lists at most one object of gvk, as metadata only, in namespace.
func listOne(ctx context.Context, c Lister, gvk schema.GroupVersionKind, namespace string) error {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	opts := []client.ListOption{client.Limit(1)}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	return c.List(ctx, list, opts...)
}

// classify maps a probe List's error to its Result.
func classify(gvk schema.GroupVersionKind, namespace string, err error) (Result, error) {
	result := Result{GVK: gvk, Namespace: namespace}
	switch {
	case err == nil:
		result.State = Available
		return result, nil
	case apierrors.IsUnauthorized(err):
		return result, fmt.Errorf("listing %s%s: the API server did not accept the credentials: %w", resourceName(gvk), where(namespace), err)
	case apierrors.IsForbidden(err):
		result.State = Forbidden
		result.Reason = fmt.Sprintf("not allowed to list %s%s", resourceName(gvk), where(namespace))
		return result, nil
	case meta.IsNoMatchError(err) || apierrors.IsNotFound(err) || isDiscoveryFailure(err):
		result.State = NotInstalled
		result.Reason = resourceName(gvk) + " is not served by this cluster"
		return result, nil
	default:
		return result, fmt.Errorf("probing %s%s: %w", resourceName(gvk), where(namespace), err)
	}
}

// ProbeConcurrency bounds how many probe Lists ProbeKinds and
// ReachableNamespaces run at once: a start-up that watches a hundred
// namespaces probes them in a few round trips instead of one after the
// other, without opening a hundred requests at once.
const ProbeConcurrency = 16

// probeEach probes gvk in every namespace, at most ProbeConcurrency at a
// time, and returns the outcomes in the order of namespaces. The first
// namespace is probed alone first: a kind the cluster does not serve is not
// served anywhere, so that one answer decides for every namespace (each one
// is then returned NotInstalled) instead of one request per namespace, which
// a real client's lazy RESTMapper sends one after the other under its lock
// (a discovery GET of the missing group each time). The first error cancels
// the probes still running and is the one returned.
func probeEach(ctx context.Context, c Lister, gvk schema.GroupVersionKind, namespaces []string) ([]Result, error) {
	results := make([]Result, len(namespaces))
	if len(namespaces) == 0 {
		return results, nil
	}
	first, err := classify(gvk, namespaces[0], listOne(ctx, c, gvk, namespaces[0]))
	if err != nil {
		return nil, err
	}
	results[0] = first
	if first.State == NotInstalled {
		for i, namespace := range namespaces {
			results[i] = first
			results[i].Namespace = namespace
		}
		return results, nil
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(ProbeConcurrency)
	for i, namespace := range namespaces[1:] {
		group.Go(func() error {
			probed, err := classify(gvk, namespace, listOne(groupCtx, c, gvk, namespace))
			results[i+1] = probed
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// Decision is the outcome of ProbeKinds: one Result per kind, in the order
// the kinds were given. No namespace is ever left out for an optional kind:
// a kind refused in some namespaces only is Available with those namespaces
// in its RefusedIn, and the feature is off there alone.
type Decision struct {
	Results []Result
}

// ProbeKinds probes each optional kind of gvks in every namespace of
// namespaces (concurrently, ProbeConcurrency at a time), or once
// cluster-wide when namespaces is empty, and decides per kind:
//
//   - not served: the kind is NotInstalled, whatever the namespace (it is a
//     fact about the cluster, found with one request);
//   - refused (Forbidden) in every namespace: the kind is Forbidden, the
//     install's own choice, and the feature is off;
//   - refused in only some of them: the kind is Available, its RefusedIn
//     names those namespaces, and the feature is off there only (Result.In,
//     Result.ListableIn): it is watched and used where it can be listed.
//
// Each kind is decided on its own and each namespace's answer decides for
// that namespace alone: one namespace's RBAC, narrowed or widened by its
// administrator, never changes what the operator does in another namespace
// (a namespace that grants a kind the install refuses everywhere else turns
// it on in that namespace only, and leaves the others watched). Only the
// required kind may leave a namespace out of the watch, and that is
// ReachableNamespaces' decision, not this one's. Any error is returned,
// whatever the other namespaces answered.
func ProbeKinds(ctx context.Context, c Lister, gvks []schema.GroupVersionKind, namespaces []string) (Decision, error) {
	decision := Decision{Results: make([]Result, len(gvks))}
	for i, gvk := range gvks {
		if len(namespaces) == 0 {
			result, err := ProbeListable(ctx, c, gvk, "")
			if err != nil {
				return Decision{}, err
			}
			decision.Results[i] = result
			continue
		}
		results, err := probeEach(ctx, c, gvk, namespaces)
		if err != nil {
			return Decision{}, err
		}
		decision.Results[i] = decide(results)
	}
	return decision, nil
}

// decide is one kind's outcome over the namespaces probed: NotInstalled when
// any namespace says so, Forbidden (the first refusal in namespace order)
// when every namespace refuses, Available otherwise (the first available
// namespace's result, with every refusing namespace in RefusedIn, in
// namespace order).
func decide(results []Result) Result {
	var refused, available *Result
	var refusedIn []string
	for i := range results {
		switch results[i].State {
		case NotInstalled:
			return results[i]
		case Forbidden:
			if refused == nil {
				refused = &results[i]
			}
			refusedIn = append(refusedIn, results[i].Namespace)
		case Available:
			if available == nil {
				available = &results[i]
			}
		}
	}
	if available == nil {
		return *refused
	}
	result := *available
	result.RefusedIn = refusedIn
	return result
}

// ReachableNamespaces splits namespaces into those where the caller may list
// gvk and those where the List is refused (Forbidden), before any feature is
// probed: a namespace the caller holds no right in (deleted after the install
// and with it its RoleBinding, or never bound) is dropped from the watch
// instead of turning every feature off everywhere or failing the start. A
// kind that is not served says nothing about the namespaces: every namespace
// is then returned reachable, for the feature probe to report. Any other error
// is returned, Unauthorized included: a 401 says the caller's credentials were
// not accepted (a token being rotated, an authentication webhook down), not
// that the namespace is out of reach, and dropping the namespace would last
// for the life of the process. Empty namespaces (cluster-wide) are returned as
// they are. The namespaces are probed concurrently, ProbeConcurrency at a
// time; both lists keep the order of namespaces.
func ReachableNamespaces(ctx context.Context, c Lister, gvk schema.GroupVersionKind, namespaces []string) (reachable []string, refused []Result, err error) {
	results, err := probeEach(ctx, c, gvk, namespaces)
	if err != nil {
		return nil, nil, err
	}
	for i, probed := range results {
		switch probed.State {
		case Available:
			reachable = append(reachable, namespaces[i])
		case Forbidden:
			refused = append(refused, probed)
		case NotInstalled:
			return append([]string(nil), namespaces...), nil, nil
		}
	}
	return reachable, refused, nil
}

// ObjectStoreGVK is the barman-cloud plugin's ObjectStore, the optional
// API an EvidenceAgent resolves its repository from.
var ObjectStoreGVK = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}

// StanzaGVK is Dalibo's pgBackRest plugin Stanza
// (pgbackrest.dalibo.com): an optional API an EvidenceAgent
// resolves a pgBackRest repository from.
var StanzaGVK = schema.GroupVersionKind{Group: "pgbackrest.dalibo.com", Version: "v1", Kind: "Stanza"}

// ArchiveGVK is Opera's pgBackRest plugin Archive
// (pgbackrest.cnpg.opera.com): an optional API an EvidenceAgent
// resolves a pgBackRest repository from.
var ArchiveGVK = schema.GroupVersionKind{Group: "pgbackrest.cnpg.opera.com", Version: "v1", Kind: "Archive"}

// isDiscoveryFailure matches the RESTMapper's report that a whole group
// version is absent, which some client versions return instead of a
// NoKindMatch.
func isDiscoveryFailure(err error) bool {
	var groupErr *meta.NoResourceMatchError
	return errors.As(err, &groupErr)
}

func resourceName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Kind
	}
	return gvk.Kind + "." + gvk.Group
}

func where(namespace string) string {
	if namespace == "" {
		return " cluster-wide"
	}
	return " in " + namespace
}

// SCCGVK is OpenShift's SecurityContextConstraints kind.
var SCCGVK = schema.GroupVersionKind{Group: "security.openshift.io", Version: "v1", Kind: "SecurityContextConstraints"}

// ProbeSCC reports whether the cluster is OpenShift, by probing the
// cluster-scoped SecurityContextConstraints kind. Forbidden counts as
// present — the API answered, it only refused this caller — which is
// exactly the answer a renderer needs to omit runAsUser and fsGroup and
// let the SCC assign them. Only "not served" means plain
// Kubernetes.
func ProbeSCC(ctx context.Context, c Lister) (bool, error) {
	result, err := ProbeListable(ctx, c, SCCGVK, "")
	if err != nil {
		return false, err
	}
	return result.State != NotInstalled, nil
}
