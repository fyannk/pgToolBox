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

package exposure

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// Remnant is what Leftover found of an exposure object an owner may have
// published before its kind was refused: the status sentence ("" when
// nothing of the owner's remains or none is known to), whether it is only
// possible (the operator could not read the object) rather than found, and
// whether nothing of the owner's is verified to remain (Gone: the record of
// the published kind can be cleared, MarkGone).
type Remnant struct {
	Sentence   string
	Unverified bool
	Gone       bool
}

// Leftover looks, past the cache, for the exposure object name (a Route or
// an HTTPRoute, kind naming it) that owner may have published in its
// namespace before the operator was refused that kind there (its
// administrator narrowed the operator's Role; the kind is watched only
// where it can be listed, so the cache holds none of it here). One owned by
// owner is deleted when remove is set (UID precondition: never an object
// recreated under the name since). It returns a fixed sentence for the
// status when such an object remains or may remain: found and kept
// (remove unset), found and its deletion refused, or unreadable (the Get
// refused) while publishedBefore says owner may have published one
// (MayHavePublished); unreadable with nothing in owner's record, it says
// nothing: a Route that was never published is not reported as one that
// may remain. The kind not served (NoKindMatch) is an error, retried: it
// was served at start (Leftover runs only for a kind the start-up probe
// found installed and refused), and an aggregated API briefly unavailable
// (openshift-apiserver restarting) answers so while the Route remains. Any
// other error is returned too. The sentence names only owner's own
// namespace and the object's name, and is the same at every loop, so a
// no-op loop writes nothing.
func Leftover(ctx context.Context, reader client.Reader, writer client.Writer, owner metav1.Object, object client.Object, kind, name string, remove, publishedBefore bool) (Remnant, error) {
	namespace := owner.GetNamespace()
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object)
	switch {
	case apierrors.IsNotFound(err):
		return Remnant{Gone: true}, nil
	case discovery.IsNotServed(err):
		return Remnant{}, fmt.Errorf("read %s %s: the API served at start is not served now (unavailable for a moment?): %w", kind, name, err)
	case apierrors.IsForbidden(err):
		if !publishedBefore {
			return Remnant{}, nil
		}
		return Remnant{
			Sentence:   fmt.Sprintf("the operator may not read %ss in %s: a %s %s published before may remain; delete it by hand", kind, namespace, kind, name),
			Unverified: true,
		}, nil
	case err != nil:
		return Remnant{}, fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	if !metav1.IsControlledBy(object, owner) {
		return Remnant{Gone: true}, nil
	}
	if !remove {
		return Remnant{Sentence: fmt.Sprintf("%s %s published before remains, no longer managed: the operator may not list %ss in %s; delete it by hand", kind, name, kind, namespace)}, nil
	}
	err = writer.Delete(ctx, object, client.Preconditions{UID: new(object.GetUID())})
	switch {
	case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
		// Conflict: the UID precondition failed, the object under the
		// name is no longer owner's.
		return Remnant{Gone: true}, nil
	case apierrors.IsForbidden(err):
		return Remnant{Sentence: fmt.Sprintf("the operator may not delete %ss in %s: %s %s published before remains; delete it by hand", kind, namespace, kind, name)}, nil
	default:
		return Remnant{}, fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
}

// MayHavePublished reports whether an owner may have published an exposure
// object of kind ("Route" or "HTTPRoute"): its spec requests that kind now
// (requested; it may have been published before the refusal), or its
// status records that the operator published one and has not seen it gone
// since (published, MarkPublished and MarkGone). A console or PgAdmin that
// was only ever clusterIP or Ingress has neither, and Leftover then
// reports nothing for a kind it may not read; one whose leftover was
// reported keeps it reported until it is gone or readable.
func MayHavePublished(requested bool, published []string, kind string) bool {
	return requested || slices.Contains(published, kind)
}

// MarkPublished records in published (a status list, kept sorted) that the
// operator published an object of kind.
func MarkPublished(published *[]string, kind string) {
	if slices.Contains(*published, kind) {
		return
	}
	*published = append(*published, kind)
	slices.Sort(*published)
}

// MarkGone records in published that nothing of kind the operator
// published remains: deleted, not found, or no longer the owner's.
func MarkGone(published *[]string, kind string) {
	*published = slices.DeleteFunc(*published, func(k string) bool { return k == kind })
	if len(*published) == 0 {
		*published = nil
	}
}

// AllUnverified reports whether every remnant is only possible (none was
// found): the confinement of what they would publish is then unknown, not
// open. False for none.
func AllUnverified(remnants []Remnant) bool {
	for _, remnant := range remnants {
		if !remnant.Unverified {
			return false
		}
	}
	return len(remnants) > 0
}

// WithLeftovers is message followed by the remnants' sentences, if any.
func WithLeftovers(message string, remnants []Remnant) string {
	var sentences []string
	for _, remnant := range remnants {
		if remnant.Sentence != "" {
			sentences = append(sentences, remnant.Sentence)
		}
	}
	if len(sentences) == 0 {
		return message
	}
	return message + "; " + strings.Join(sentences, "; ")
}
