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

// Package v1alpha1 contains the API Schema definitions of the operator in
// the pgtoolbox.fyannk.dev group: PgAdmin, EvidenceAgent, PgToolBoxConsole,
// PgAccessRequest and PgAdminAccess.
//
// File convention: groupversion_info.go, conditions.go and common_types.go
// are shared; every kind lives in its own <kind>_types.go (markers and types only) and
// <kind>_funcs.go (methods), and registers itself in its own init() with
// SchemeBuilder.Register, so adding a kind never edits a shared file.
// Kind-specific condition reasons stay in the kind's own files and join the
// frozen catalogue through registerReasons (conditions.go).
//
// +kubebuilder:object:generate=true
// +groupName=pgtoolbox.fyannk.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "pgtoolbox.fyannk.dev", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	// Each kind registers itself from its own init().
	SchemeBuilder = &Builder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Builder collects the kinds of this group-version and adds them to a
// scheme. It does what controller-runtime's deprecated scheme.Builder did
// (known types plus the group-version's meta types) with apimachinery
// alone, so importing the API package pulls in no controller-runtime.
//
// +kubebuilder:object:generate=false
type Builder struct {
	objects []runtime.Object
}

// Register adds kinds to the group-version; each kind calls it from its
// own init().
func (b *Builder) Register(objects ...runtime.Object) {
	b.objects = append(b.objects, objects...)
}

// AddToScheme adds every registered kind, and the meta types of the
// group-version, to s.
func (b *Builder) AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, b.objects...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
