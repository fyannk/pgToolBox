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

package pgadmin

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestCollectorComputesGaugesFromTheCache: pgtoolbox_pgadmin_info and
// pgtoolbox_pgadmin_condition with their fixed labels, computed
// from the cached PgAdmins, through a registry as the operator registers
// them.
func TestCollectorComputesGaugesFromTheCache(t *testing.T) {
	p := oauth2PgAdmin()
	p.Status.Image = testPgAdminImage
	p.Status.Servers = []pgtoolboxv1alpha1.PgAdminServerStatus{
		{Cluster: "orders", Endpoint: "rw", User: "app"},
		{Cluster: "orders", Endpoint: "ro", User: "app"},
		{Cluster: "billing", Endpoint: "rw", User: "app"},
	}
	p.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AsExpected"},
		{Type: "PgAdminEgressConfined", Status: metav1.ConditionFalse, Reason: "PgAdminEgressOpen"},
	}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).WithStatusSubresource(p).Build()
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(Collector{Reader: reader}); err != nil {
		t.Fatal(err)
	}
	expected := `
# HELP pgtoolbox_pgadmin_condition PgAdmin conditions: 1 for the current status of each condition type.
# TYPE pgtoolbox_pgadmin_condition gauge
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="False",type="PgAdminEgressConfined"} 1
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="False",type="Ready"} 0
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="True",type="PgAdminEgressConfined"} 0
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="True",type="Ready"} 1
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="Unknown",type="PgAdminEgressConfined"} 0
pgtoolbox_pgadmin_condition{name="team",namespace="team-a",status="Unknown",type="Ready"} 0
# HELP pgtoolbox_pgadmin_info One series per PgAdmin: how many Clusters it registers and the pgAdmin image it runs.
# TYPE pgtoolbox_pgadmin_info gauge
pgtoolbox_pgadmin_info{clusters="2",image="` + testPgAdminImage + `",name="team",namespace="team-a"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
