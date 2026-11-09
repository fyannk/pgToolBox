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

package metadata

import (
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

func TestTransformSecretMetadata(t *testing.T) {
	secretData := `{"apiVersion":"v1","kind":"Secret","data":{"password":"aHVudGVyMg=="}}`
	owner := metav1.OwnerReference{APIVersion: "pgtoolbox.fyannk.dev/v1alpha1", Kind: "PgAdmin", Name: "team", UID: "u1"}
	cases := []struct {
		name       string
		labels     map[string]string
		annotation map[string]string
		wantLabels map[string]string
	}{
		{
			name:       "last-applied and user labels dropped",
			labels:     map[string]string{"team": "a", "app.kubernetes.io/name": "x"},
			annotation: map[string]string{lastApplied: secretData, "other": "y"},
		},
		{
			name: "managed-by and reload kept",
			labels: map[string]string{
				pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue,
				pgtoolboxv1alpha1.LabelReload:    pgtoolboxv1alpha1.ReloadLabelValue,
				"team":                           "a",
			},
			annotation: map[string]string{lastApplied: secretData},
			wantLabels: map[string]string{
				pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue,
				pgtoolboxv1alpha1.LabelReload:    pgtoolboxv1alpha1.ReloadLabelValue,
			},
		},
		{name: "no metadata at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, in := range []metav1.Object{
				&metav1.PartialObjectMetadata{ObjectMeta: objectMeta(tc.labels, tc.annotation, owner)},
				&corev1.Secret{ObjectMeta: objectMeta(tc.labels, tc.annotation, owner)},
			} {
				out, err := TransformSecretMetadata()(in)
				if err != nil {
					t.Fatal(err)
				}
				got := out.(metav1.Object)
				if len(got.GetAnnotations()) != 0 {
					t.Errorf("%T: annotations kept: %v", in, got.GetAnnotations())
				}
				if !maps.Equal(got.GetLabels(), tc.wantLabels) {
					t.Errorf("%T: labels = %v, want %v", in, got.GetLabels(), tc.wantLabels)
				}
				if got.GetManagedFields() != nil {
					t.Errorf("%T: managedFields kept", in)
				}
				if got.GetName() != "s" || got.GetNamespace() != "team-a" || got.GetResourceVersion() != "7" ||
					len(got.GetOwnerReferences()) != 1 || got.GetOwnerReferences()[0].UID != "u1" {
					t.Errorf("%T: identity or owner references lost: %+v", in, got)
				}
			}
		})
	}
}

// TestTransformSecretMetadataTombstone passes a tombstone through: it holds
// an object transformed when it was added.
func TestTransformSecretMetadataTombstone(t *testing.T) {
	tombstone := toolscache.DeletedFinalStateUnknown{Key: "team-a/s"}
	out, err := TransformSecretMetadata()(tombstone)
	if err != nil || out != any(tombstone) {
		t.Fatalf("tombstone = %v, %v", out, err)
	}
}

func objectMeta(labels, annotations map[string]string, owner metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: "s", Namespace: "team-a", ResourceVersion: "7",
		Labels: maps.Clone(labels), Annotations: maps.Clone(annotations),
		OwnerReferences: []metav1.OwnerReference{owner},
		ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "kubectl", FieldsV1: metav1.NewFieldsV1(strings.Repeat("x", 8))}},
	}
}
