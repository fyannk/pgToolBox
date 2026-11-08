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

func TestOperationHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase   viewmodel.OperationPhase
		blocked bool
		want    viewmodel.Health
	}{
		{viewmodel.OpPhaseRequested, false, viewmodel.HealthProgressing},
		{viewmodel.OpPhaseAccepted, false, viewmodel.HealthProgressing},
		{viewmodel.OpPhaseActing, false, viewmodel.HealthProgressing},
		{viewmodel.OpPhaseActing, true, viewmodel.HealthWarning},
		{viewmodel.OpPhaseAccepted, true, viewmodel.HealthWarning},
		{viewmodel.OpPhaseSettled, false, viewmodel.HealthOK},
		// A settled operation is ok even if a stale blocked flag lingers.
		{viewmodel.OpPhaseSettled, true, viewmodel.HealthOK},
		{viewmodel.OpPhaseFailed, false, viewmodel.HealthCritical},
		// Timed out is not failed: the console saw no outcome.
		{viewmodel.OpPhaseTimedOut, false, viewmodel.HealthUnknown},
		{viewmodel.OpPhaseNotObservable, false, viewmodel.HealthInfo},
		// Never ok for a phase the console does not know.
		{viewmodel.OperationPhase("bogus"), false, viewmodel.HealthUnknown},
	}
	for _, tt := range tests {
		if got := viewmodel.OperationHealth(tt.phase, tt.blocked); got != tt.want {
			t.Errorf("OperationHealth(%q, %v) = %q, want %q", tt.phase, tt.blocked, got, tt.want)
		}
	}
}
