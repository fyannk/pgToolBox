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

package evidence

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// OperatorStatusManager is the field manager of the operator's status
// writes (internal/operator/reconcile.StatusFieldOwner; the console may
// not import the operator, so a test of the operator pins the two equal).
const OperatorStatusManager = "pgtoolbox-status"

// trustedStatusFields are the status fields the console acts on before it
// has any answer from the agent: where to send its token, and which CA
// proves the server it reached.
var trustedStatusFields = []string{"f:endpoint", "f:caBundle"}

// StatusWrittenByOperator reports whether every field manager that owns
// status.endpoint or status.caBundle is the operator's, and the operator
// owns both. It reads managedFields, which the console's cache keeps for
// EvidenceAgents only, and only for the status subresource (CacheTransform).
//
// It is defense in depth, not authentication: a field manager's name is
// asserted by the client that writes, so whoever may patch
// evidenceagents/status can claim OperatorStatusManager. The check catches
// a status written by any other tool or by hand; the per-namespace token
// audience is what bounds a deliberate impersonation to its own namespace.
// An object without managedFields (a cache that stripped them) fails the
// check: the console never sends its token to a status it cannot place.
func StatusWrittenByOperator(agent *pgtoolboxv1alpha1.EvidenceAgent) bool {
	owned := map[string]bool{}
	for _, entry := range agent.ManagedFields {
		if entry.FieldsV1 == nil || entry.Subresource != "status" {
			continue
		}
		fields := statusFieldsOf(entry.FieldsV1.GetRawBytes())
		for _, field := range trustedStatusFields {
			if !fields[field] {
				continue
			}
			if entry.Manager != OperatorStatusManager {
				return false
			}
			owned[field] = true
		}
	}
	for _, field := range trustedStatusFields {
		if !owned[field] {
			return false
		}
	}
	return true
}

// statusFieldsOf returns the top-level keys under "f:status" of a FieldsV1
// set; an undecodable set owns nothing.
func statusFieldsOf(raw []byte) map[string]bool {
	var set struct {
		Status map[string]json.RawMessage `json:"f:status"`
	}
	if json.Unmarshal(raw, &set) != nil {
		return nil
	}
	fields := make(map[string]bool, len(set.Status))
	for key := range set.Status {
		fields[key] = true
	}
	return fields
}

// CacheTransform is the console cache's transform for EvidenceAgents: it
// keeps only the managedFields entries of the status subresource, which
// StatusWrittenByOperator reads, and drops every other entry as the
// default transform does for every other kind. Objects that are not
// EvidenceAgents (tombstones included) pass through unchanged.
func CacheTransform(object any) (any, error) {
	if tombstone, ok := object.(cache.DeletedFinalStateUnknown); ok {
		return tombstone, nil
	}
	accessor, ok := object.(metav1.Object)
	if !ok {
		return object, nil
	}
	if _, isAgent := object.(*pgtoolboxv1alpha1.EvidenceAgent); !isAgent {
		accessor.SetManagedFields(nil)
		return object, nil
	}
	var kept []metav1.ManagedFieldsEntry
	for _, entry := range accessor.GetManagedFields() {
		if entry.Subresource == "status" {
			kept = append(kept, entry)
		}
	}
	accessor.SetManagedFields(kept)
	return object, nil
}
