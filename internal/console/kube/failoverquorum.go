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

// convertFailoverQuorum converts a raw quorum object into facts. The
// standby list is bounded here, at the boundary, so no later layer can
// render or retain an unbounded one.
func convertFailoverQuorum(content map[string]any) (observe.FailoverQuorumFacts, error) {
	var quorum cnpgv1.FailoverQuorum
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &quorum); err != nil {
		return observe.FailoverQuorumFacts{}, redact.NewError("failover quorum convert", redact.CategoryInternal, err)
	}
	facts := observe.FailoverQuorumFacts{
		Present:       true,
		Method:        quorum.Status.Method,
		Primary:       quorum.Status.Primary,
		StandbyNumber: quorum.Status.StandbyNumber,
	}
	standbys := quorum.Status.StandbyNames
	if len(standbys) > observe.MaxQuorumStandbys {
		standbys = standbys[:observe.MaxQuorumStandbys]
		facts.StandbysTruncated = true
	}
	facts.Standbys = append([]string(nil), standbys...)
	return facts, nil
}
