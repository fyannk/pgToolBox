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
	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// convertPooler converts a raw Pooler into facts plus its membership
// verdict. Membership is the exact spec.cluster.name reference in the
// configured namespace.
//
// status.secrets is deliberately not read: it names credential Secrets,
// the console holds no Secret permission, and a pooler's credential
// wiring is not something this screen answers.
func (c *Client) convertPooler(content map[string]any) (observe.PoolerFacts, bool, error) {
	var pooler cnpgv1.Pooler
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &pooler); err != nil {
		return observe.PoolerFacts{}, false, redact.NewError("pooler convert", redact.CategoryInternal, err)
	}
	facts := observe.PoolerFacts{
		Name:             pooler.Name,
		UID:              string(pooler.UID),
		Type:             string(pooler.Spec.Type),
		DesiredInstances: pooler.Spec.Instances,
		ReadyInstances:   pooler.Status.Instances,
		Phase:            string(pooler.Status.Phase),
		PhaseReason:      boundOperatorMessage(pooler.Status.PhaseReason),
		Image:            pooler.Status.Image,
		CreatedAt:        pooler.CreationTimestamp.Time.UTC(),
	}
	if pooler.Spec.PgBouncer != nil {
		facts.PoolMode = string(pooler.Spec.PgBouncer.PoolMode)
	}
	return facts, pooler.Namespace == c.opts.Namespace && pooler.Spec.Cluster.Name == c.opts.ClusterName, nil
}
