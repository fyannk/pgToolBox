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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Leftover looks, past the cache, for the exposure object name (a Route or
// an HTTPRoute, kind naming it) that owner may have published in its
// namespace before the operator was refused that kind there (its
// administrator narrowed the operator's Role; the kind is watched only
// where it can be listed, so the cache holds none of it here). One owned by
// owner is deleted when remove is set (UID precondition: never an object
// recreated under the name since). It returns a fixed sentence for the
// status when such an object remains or may remain: found and kept
// (remove unset), found and its deletion refused, or unreadable (the Get
// refused). "" when nothing of owner's is there. Any other error is
// returned. The sentence names only owner's own namespace and the object's
// name, and is the same at every loop, so a no-op loop writes nothing.
func Leftover(ctx context.Context, reader client.Reader, writer client.Writer, owner metav1.Object, object client.Object, kind, name string, remove bool) (string, error) {
	namespace := owner.GetNamespace()
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case apierrors.IsForbidden(err):
		return fmt.Sprintf("the operator may not read %ss in %s: a %s %s published before may remain; delete it by hand", kind, namespace, kind, name), nil
	case err != nil:
		return "", fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	if !metav1.IsControlledBy(object, owner) {
		return "", nil
	}
	if !remove {
		return fmt.Sprintf("%s %s published before remains, no longer managed: the operator may not list %ss in %s; delete it by hand", kind, name, kind, namespace), nil
	}
	err = writer.Delete(ctx, object, client.Preconditions{UID: new(object.GetUID())})
	switch {
	case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
		// Conflict: the UID precondition failed, the object under the
		// name is no longer owner's.
		return "", nil
	case apierrors.IsForbidden(err):
		return fmt.Sprintf("the operator may not delete %ss in %s: %s %s published before remains; delete it by hand", kind, namespace, kind, name), nil
	default:
		return "", fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
}

// WithLeftovers is message followed by the Leftover sentences, if any.
func WithLeftovers(message string, leftovers []string) string {
	if len(leftovers) == 0 {
		return message
	}
	return message + "; " + strings.Join(leftovers, "; ")
}
