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

package status

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

type runningDiagnostics struct{}

func (runningDiagnostics) Latest(cache.ClusterKey) (diag.Run, bool) { return diag.Run{}, false }
func (runningDiagnostics) State() viewmodel.SourceState {
	return viewmodel.SourceState{State: viewmodel.SourceLive}
}

// A high finding is counted in the overview's Diagnostics line and turns it
// red: the line must agree with the findings counter beside it.
func TestDiagnosticsContributorCountsHigh(t *testing.T) {
	t.Parallel()
	run := &diag.Run{At: time.Now(), Result: diagnose.Result{Findings: []diagnose.Finding{
		{ID: "a", Severity: diagnose.SeverityHigh},
		{ID: "b", Severity: diagnose.SeverityWarning},
	}}}
	c := diagnosticsContributor(run, runningDiagnostics{})
	if c.Health != viewmodel.HealthCritical {
		t.Errorf("health = %s, want critical", c.Health)
	}
	if !strings.HasPrefix(c.Detail, "0 critical, 1 high, 1 warning, 0 note") {
		t.Errorf("detail = %q", c.Detail)
	}
}
