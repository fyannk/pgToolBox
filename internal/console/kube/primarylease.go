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

package kube

import (
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// convertPrimaryLease reads the Lease spec verbatim. Every field is
// optional in the API and stays nil when absent.
func convertPrimaryLease(content map[string]any) (observe.PrimaryLeaseFacts, error) {
	var lease coordinationv1.Lease
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &lease); err != nil {
		return observe.PrimaryLeaseFacts{}, redact.NewError("primary lease convert", redact.CategoryInternal, err)
	}
	facts := observe.PrimaryLeaseFacts{
		Present:         true,
		Name:            lease.Name,
		DurationSeconds: lease.Spec.LeaseDurationSeconds,
		Transitions:     lease.Spec.LeaseTransitions,
	}
	if lease.Spec.HolderIdentity != nil {
		facts.Holder = boundOperatorMessage(*lease.Spec.HolderIdentity)
	}
	if lease.Spec.AcquireTime != nil {
		at := lease.Spec.AcquireTime.Time.UTC()
		facts.AcquiredAt = &at
	}
	if lease.Spec.RenewTime != nil {
		at := lease.Spec.RenewTime.Time.UTC()
		facts.RenewedAt = &at
	}
	return facts, nil
}
