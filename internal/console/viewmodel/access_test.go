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

package viewmodel_test

import (
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestAccessHealth(t *testing.T) {
	granted := viewmodel.AccessCondition{Status: "True", Reason: "AsExpected", Observed: true}
	missing := viewmodel.AccessCondition{Status: "False", Reason: "RoleBindingMissing", Observed: true}
	unobserved := viewmodel.AccessCondition{Status: "Unknown", Reason: "Initializing"}
	cases := []struct {
		phase   string
		c       viewmodel.AccessCondition
		granted bool
		want    viewmodel.Health
	}{
		{"Pending", unobserved, false, viewmodel.HealthProgressing},
		{"Pending", granted, false, viewmodel.HealthProgressing},
		{"Approved", granted, true, viewmodel.HealthOK},
		{"Approved", granted, false, viewmodel.HealthProgressing},
		{"Approved", unobserved, false, viewmodel.HealthProgressing},
		{"Approved", viewmodel.AccessCondition{Status: "True", Observed: false}, false, viewmodel.HealthProgressing},
		{"Approved", missing, false, viewmodel.HealthWarning},
		{"Denied", granted, false, viewmodel.HealthInactive},
		{"Expired", unobserved, false, viewmodel.HealthInactive},
		{"Revoked", granted, false, viewmodel.HealthInactive},
		{"Revoked", viewmodel.AccessCondition{Status: "False", Reason: "ReconcileError"}, false, viewmodel.HealthWarning},
		{"Pending", viewmodel.AccessCondition{Status: "False", Reason: "ClusterNotFound"}, false, viewmodel.HealthWarning},
		{"Bogus", granted, false, viewmodel.HealthUnknown},
	}
	for _, tc := range cases {
		got := viewmodel.AccessHealth(tc.phase, tc.c, tc.granted)
		if got != tc.want || !got.Valid() {
			t.Errorf("AccessHealth(%s, %s %s) = %s, want %s", tc.phase, tc.c.Status, tc.c.Reason, got, tc.want)
		}
	}
}
