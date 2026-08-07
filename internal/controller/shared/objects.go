/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package shared

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControllerOwnerMatches reports whether both objects share the same
// controller owner UID.
func ControllerOwnerMatches(existing, desired metav1.Object) bool {
	existingOwner := metav1.GetControllerOf(existing)
	desiredOwner := metav1.GetControllerOf(desired)
	return existingOwner != nil && desiredOwner != nil && existingOwner.UID == desiredOwner.UID
}

// LabelsContain reports whether every desired key/value pair is present.
func LabelsContain(existing, desired map[string]string) bool {
	for key, value := range desired {
		if existing[key] != value {
			return false
		}
	}
	return true
}
