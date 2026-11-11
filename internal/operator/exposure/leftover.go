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
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// Remnant is what Leftover found of an exposure object an owner may have
// published before its kind was refused: the status sentence ("" when
// nothing of the owner's remains), and whether it is only possible (the
// operator could not read the object) rather than found.
type Remnant struct {
	Sentence   string
	Unverified bool
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
// (MayHavePublished); unreadable with nothing in owner's history, it says
// nothing: a Route that was never published is not reported as one that
// may remain. Nothing either when the kind is no longer served (its CRD
// removed while the operator runs: nothing of it can remain). Any other
// error is returned. The sentence names only owner's own namespace and the
// object's name, and is the same at every loop, so a no-op loop writes
// nothing.
func Leftover(ctx context.Context, reader client.Reader, writer client.Writer, owner metav1.Object, object client.Object, kind, name string, remove, publishedBefore bool) (Remnant, error) {
	namespace := owner.GetNamespace()
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object)
	switch {
	case apierrors.IsNotFound(err), discovery.IsNotServed(err):
		return Remnant{}, nil
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
		return Remnant{}, nil
	}
	if !remove {
		return Remnant{Sentence: fmt.Sprintf("%s %s published before remains, no longer managed: the operator may not list %ss in %s; delete it by hand", kind, name, kind, namespace)}, nil
	}
	err = writer.Delete(ctx, object, client.Preconditions{UID: new(object.GetUID())})
	switch {
	case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err), discovery.IsNotServed(err):
		// Conflict: the UID precondition failed, the object under the
		// name is no longer owner's.
		return Remnant{}, nil
	case apierrors.IsForbidden(err):
		return Remnant{Sentence: fmt.Sprintf("the operator may not delete %ss in %s: %s %s published before remains; delete it by hand", kind, namespace, kind, name)}, nil
	default:
		return Remnant{}, fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
}

// kindMentions is, per exposure kind Leftover looks for, the words a
// status message of the operator uses only about an object of that kind:
// its observed fact (admitted by a router, accepted by a Gateway), "not
// observed yet", "API unavailable", and Leftover's own sentences. Whole
// words: "Route" never matches "HTTPRoute" or "Routes".
var kindMentions = map[string]*regexp.Regexp{
	"Route":     regexp.MustCompile(`\b(Route|router)\b`),
	"HTTPRoute": regexp.MustCompile(`\b(HTTPRoute|Gateway)\b`),
}

// MayHavePublished reports whether an owner's history says it may have
// published an exposure object of kind ("Route" or "HTTPRoute"): its spec
// requests that kind now (requested; an earlier operator may have
// published it before the refusal), or a condition it carries from an
// earlier loop speaks of one (the observed Route or HTTPRoute, or a
// Leftover sentence about it, so a reported leftover stays reported until
// it is gone or readable). A console or PgAdmin that was only ever
// clusterIP or Ingress has none, and Leftover then reports nothing for a
// kind it may not read.
func MayHavePublished(requested bool, conditions []metav1.Condition, kind string) bool {
	if requested {
		return true
	}
	mentions, ok := kindMentions[kind]
	if !ok {
		return true
	}
	for _, condition := range conditions {
		if mentions.MatchString(condition.Message) {
			return true
		}
	}
	return false
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
