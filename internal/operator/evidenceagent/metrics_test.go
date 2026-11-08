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

package evidenceagent

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestCollectorComputesGaugesFromTheCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := testAgent()
	agent.Status.Image = "registry.example/pgtoolbox-evidence-agent@sha256:0"
	agent.Status.ProducerEpoch = 3
	agent.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AsExpected"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
	expected := `
# HELP pgtoolbox_evidenceagent_condition EvidenceAgent conditions: 1 for the current status of each condition type.
# TYPE pgtoolbox_evidenceagent_condition gauge
pgtoolbox_evidenceagent_condition{name="orders",namespace="team-a",status="False",type="Ready"} 0
pgtoolbox_evidenceagent_condition{name="orders",namespace="team-a",status="True",type="Ready"} 1
pgtoolbox_evidenceagent_condition{name="orders",namespace="team-a",status="Unknown",type="Ready"} 0
# HELP pgtoolbox_evidenceagent_info One series per EvidenceAgent: the Cluster it reads, the issuer and the agent image.
# TYPE pgtoolbox_evidenceagent_info gauge
pgtoolbox_evidenceagent_info{cluster="orders",image="registry.example/pgtoolbox-evidence-agent@sha256:0",issuer="Operator",name="orders",namespace="team-a"} 1
# HELP pgtoolbox_evidenceagent_producer_epoch The producer epoch the running agent publishes (status.producerEpoch).
# TYPE pgtoolbox_evidenceagent_producer_epoch gauge
pgtoolbox_evidenceagent_producer_epoch{name="orders",namespace="team-a"} 3
`
	if err := testutil.CollectAndCompare(Collector{Reader: reader}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// TestControllerOptions on the EvidenceAgent controller: warmed-up sources, a reconcile timeout, more than one worker.
func TestControllerOptions(t *testing.T) {
	options := controllerOptions()
	if options.EnableWarmup == nil || !*options.EnableWarmup {
		t.Error("EnableWarmup is off")
	}
	if options.ReconciliationTimeout <= 0 || options.MaxConcurrentReconciles < 2 {
		t.Errorf("options = %+v", options)
	}
}
