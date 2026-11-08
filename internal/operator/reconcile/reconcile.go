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

// Package reconcile holds the machinery every controller of the operator shares: the
// ErrNextLoop sentinel, server-side apply with forced ownership and a
// controlled-by check, error classification into the frozen reason
// catalogue and bounded messages. Optional APIs are probed with
// pkg/discovery, shared with the console.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// FieldOwner is the server-side apply field manager of every child object
// the operator writes.
const FieldOwner = "pgtoolbox"

// StatusFieldOwner is the field manager of status writes.
const StatusFieldOwner = "pgtoolbox-status"

// ErrNextLoop says a concern is waiting for something a watch will report
// (a Pod becoming Ready, a Job finishing): the reconcile stops there,
// publishes status, and returns no error — the event, not a requeue,
// brings the next loop.
var ErrNextLoop = errors.New("waiting for the next reconciliation")

// NextLoop wraps ErrNextLoop with what is awaited.
func NextLoop(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrNextLoop)
}

// ErrStalled marks a loop that stopped on a problem only a change of
// input fixes (a missing referenced object, a foreign object under a
// generated name): Stalled=True, Progressing=False, and no error returned
// to controller-runtime — retrying cannot help, the watch on the input
// brings the next loop.
var ErrStalled = errors.New("stalled")

// StallError is ErrStalled with the reason and message Stalled reports.
type StallError struct {
	Reason  pgtoolboxv1alpha1.Reason
	Message string
}

func (e *StallError) Error() string { return "stalled: " + e.Message }

// Is makes errors.Is(err, ErrStalled) hold for every StallError.
func (e *StallError) Is(target error) bool { return target == ErrStalled }

// Stall returns a StallError.
func Stall(reason pgtoolboxv1alpha1.Reason, format string, args ...any) error {
	return &StallError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// NotOwnedError reports an object under a generated name that something
// else controls. It is surfaced as ConfigurationValid=False
// (ResourceNotOwned), never overwritten. Namespace is empty for a
// cluster-scoped object.
type NotOwnedError struct {
	Kind, Namespace, Name string
}

func (e *NotOwnedError) Error() string {
	if e.Namespace == "" {
		return fmt.Sprintf("%s %s exists and is not managed by pgtoolbox; rename or delete it", e.Kind, e.Name)
	}
	return fmt.Sprintf("%s %s/%s exists and is not controlled by this resource; rename or delete it",
		e.Kind, e.Namespace, e.Name)
}

// Applier writes child objects with server-side apply and forced ownership.
// The API server computes the diff, so a no-op apply is a no-op and no
// hand-written drift matcher exists to forget a field.
type Applier struct {
	// Client reads through the label-filtered cache and writes to the API.
	Client client.Client
	// APIReader reads past the cache. The cache holds only objects that
	// carry the operator's labels, so an object of the same name created
	// by someone else is invisible to it; before creating, the Applier
	// asks the API server, so it never takes over such an object.
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

// Apply applies desired, after checking that an existing object of the same
// name is controlled by the same owner (desired's controller reference).
// An object of that name without that controller is never taken over:
// labels are predictable, so anyone who may create the kind could plant one
// (a Deployment with an extra container, a Secret with a known value).
// desired must carry its TypeMeta.
func (a Applier) Apply(ctx context.Context, desired client.Object) error {
	return a.apply(ctx, desired, false)
}

// ApplyAdopting is Apply that also adopts an existing object with no
// controller when it carries desired's identity labels: only for a
// PersistentVolumeClaim kept by retainPolicy Retain, which its PgAdmin must
// find again after being recreated. Creating such a claim takes nothing a
// Pod creator in the namespace does not already have.
func (a Applier) ApplyAdopting(ctx context.Context, desired client.Object) error {
	return a.apply(ctx, desired, true)
}

func (a Applier) apply(ctx context.Context, desired client.Object, adopt bool) error {
	if err := a.checkControl(ctx, desired, adopt); err != nil {
		return err
	}
	raw, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("marshal %T apply patch: %w", desired, err)
	}
	if err := a.Client.Patch(ctx, desired, client.RawPatch(types.ApplyPatchType, raw),
		client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", kindOf(a.Scheme, desired), desired.GetNamespace(), desired.GetName(), err)
	}
	return nil
}

func (a Applier) checkControl(ctx context.Context, desired client.Object, adopt bool) error {
	gvk, err := apiutil.GVKForObject(desired, a.Scheme)
	if err != nil {
		return err
	}
	if gvk.Group == "" && gvk.Kind == "Secret" {
		// Never a typed read of a Secret: its metadata, past the cache.
		existing := &metav1.PartialObjectMetadata{}
		existing.SetGroupVersionKind(gvk)
		if err := a.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !Controls(existing, desired, adopt) {
			return &NotOwnedError{Kind: gvk.Kind, Namespace: desired.GetNamespace(), Name: desired.GetName()}
		}
		return nil
	}
	// A typed read, so it is served by the informer the controller already
	// runs for this kind (label-filtered); a metadata-only read would start
	// a second informer.
	fresh, err := a.Scheme.New(gvk)
	if err != nil {
		return err
	}
	existing, ok := fresh.(client.Object)
	if !ok {
		return fmt.Errorf("%s is not a client.Object", gvk)
	}
	err = a.Client.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) && a.APIReader != nil {
		live := &metav1.PartialObjectMetadata{}
		live.SetGroupVersionKind(gvk)
		err = a.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), live)
		existing = live
	}
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if !Controls(existing, desired, adopt) {
		return &NotOwnedError{Kind: gvk.Kind, Namespace: desired.GetNamespace(), Name: desired.GetName()}
	}
	return nil
}

// Controls reports whether existing belongs to desired's owner: the same
// controller UID. With adopt, an existing object without any controller
// also belongs when it carries every identity label desired carries
// (managed-by and the operator's own keys); a desired object without a
// controller (a retained volume) is matched by those labels alone.
func Controls(existing, desired metav1.Object, adopt bool) bool {
	want, got := metav1.GetControllerOf(desired), metav1.GetControllerOf(existing)
	switch {
	case got != nil && want != nil:
		return got.UID == want.UID
	case got != nil:
		return false
	case want != nil && !adopt:
		return false
	}
	labels := existing.GetLabels()
	identified := false
	for key, value := range desired.GetLabels() {
		if key != pgtoolboxv1alpha1.LabelManagedBy && !strings.HasPrefix(key, pgtoolboxv1alpha1.ReservedKeyPrefix) {
			continue
		}
		identified = true
		if labels[key] != value {
			return false
		}
	}
	return identified
}

func kindOf(scheme *runtime.Scheme, obj runtime.Object) string {
	if gvk, err := apiutil.GVKForObject(obj, scheme); err == nil {
		return gvk.Kind
	}
	return fmt.Sprintf("%T", obj)
}

// MaxMessageLength bounds every condition message and Event note.
const MaxMessageLength = 1024

// BoundedMessage cuts text to MaxMessageLength on a rune boundary.
func BoundedMessage(text string) string {
	if len(text) <= MaxMessageLength {
		return text
	}
	const ellipsis = "..."
	cut := MaxMessageLength - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + ellipsis
}

// ErrorReason classifies an error that ended a reconciliation into the
// reason of its Warning Event, or "" when the error is not worth one:
// cancellation, a conflict (the next loop has fresher data), ErrNextLoop,
// a stall, or a terminal error already reported through a condition. A
// write the API server rejected as Invalid (schema, CEL) and a Forbidden
// from an admission plugin are AdmissionDenied; a quota refusal is
// QuotaExceeded.
func ErrorReason(err error) pgtoolboxv1alpha1.Reason {
	if err == nil ||
		errors.Is(err, ErrNextLoop) ||
		errors.Is(err, ErrStalled) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		apierrors.IsConflict(err) ||
		errors.Is(err, ctrlreconcile.TerminalError(nil)) {
		return ""
	}
	if apierrors.IsInvalid(err) {
		return pgtoolboxv1alpha1.ReasonAdmissionDenied
	}
	if apierrors.IsForbidden(err) {
		message := err.Error()
		switch {
		case strings.Contains(message, "exceeded quota"), strings.Contains(message, "failed quota"):
			return pgtoolboxv1alpha1.ReasonQuotaExceeded
		case strings.Contains(message, "denied the request"), strings.Contains(message, "violates PodSecurity"):
			return pgtoolboxv1alpha1.ReasonAdmissionDenied
		}
	}
	return pgtoolboxv1alpha1.ReasonReconciliationFailed
}
