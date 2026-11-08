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

package accessrequest

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestCollectorComputesGaugesFromTheCache(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(time.Unix(1_800_000_000, 0)))
	r.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AsExpected"}}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(r).WithStatusSubresource(r).Build()
	expected := `
# HELP pgtoolbox_pgaccessrequest_condition PgAccessRequest conditions: 1 for the current status of each condition type.
# TYPE pgtoolbox_pgaccessrequest_condition gauge
pgtoolbox_pgaccessrequest_condition{name="jane-operator",namespace="team-a",status="False",type="Ready"} 0
pgtoolbox_pgaccessrequest_condition{name="jane-operator",namespace="team-a",status="True",type="Ready"} 1
pgtoolbox_pgaccessrequest_condition{name="jane-operator",namespace="team-a",status="Unknown",type="Ready"} 0
# HELP pgtoolbox_pgaccessrequest_expires_timestamp_seconds When an approved request's grant ends (spec.expiresAt), for requests that have one.
# TYPE pgtoolbox_pgaccessrequest_expires_timestamp_seconds gauge
pgtoolbox_pgaccessrequest_expires_timestamp_seconds{name="jane-operator",namespace="team-a"} 1.8e+09
# HELP pgtoolbox_pgaccessrequest_info One series per PgAccessRequest: its capability, phase and subject kind.
# TYPE pgtoolbox_pgaccessrequest_info gauge
pgtoolbox_pgaccessrequest_info{capability="operator",name="jane-operator",namespace="team-a",phase="Approved",subject_kind="User"} 1
`
	if err := testutil.CollectAndCompare(Collector{Reader: reader}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// TestControllerOptions: warmed-up sources, a
// reconcile timeout, more than one worker.
func TestControllerOptions(t *testing.T) {
	options := controllerOptions()
	if options.EnableWarmup == nil || !*options.EnableWarmup {
		t.Error("EnableWarmup is off")
	}
	if options.ReconciliationTimeout <= 0 || options.MaxConcurrentReconciles < 2 {
		t.Errorf("options = %+v", options)
	}
}
