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

package diagnose

import (
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func storeInput(ref observe.ObjectStoreReference, schedules ...observe.ScheduledBackupFacts) Input {
	return Input{Now: now, HasBackups: true, Backups: observe.BackupsSnapshot{
		ObjectStore: ref, ScheduledBackups: schedules,
	}}
}

// TestStoreBackupFailedComparesTheTwoInstants proves the plugin's last
// failure counts only when it is the more recent of the two, and that a
// store the console cannot read leaves the check unable to run while
// a cluster with no store leaves it clear.
func TestStoreBackupFailedComparesTheTwoInstants(t *testing.T) {
	t.Parallel()
	earlier, later := now.Add(-2*time.Hour), now.Add(-time.Hour)
	present := observe.ObjectStoreReference{Name: "orders-store", ServerName: "orders", State: observe.ObjectStorePresent}
	for name, tc := range map[string]struct {
		window  *observe.RecoveryWindow
		outcome CheckOutcome
	}{
		"failure after success":   {&observe.RecoveryWindow{LastSuccessfulBackup: &earlier, LastFailedBackup: &later}, CheckMatched},
		"success after failure":   {&observe.RecoveryWindow{LastSuccessfulBackup: &later, LastFailedBackup: &earlier}, CheckClear},
		"failure with no success": {&observe.RecoveryWindow{LastFailedBackup: &later}, CheckMatched},
		"no window yet":           {nil, CheckClear},
	} {
		ref := present
		ref.RecoveryWindow = tc.window
		check, findings := evaluateOnce(t, StoreBackupFailed{}, storeInput(ref))
		if check.Outcome != tc.outcome {
			t.Errorf("%s: %v %q, want %v", name, check.Outcome, check.Because, tc.outcome)
		}
		if tc.outcome == CheckMatched && (findings[0].Subject != (EntityRef{Kind: "ObjectStore", Name: "orders-store"}) || !findings[0].At.Equal(later)) {
			t.Errorf("%s: finding = %+v", name, findings[0])
		}
	}
	unknown := observe.ObjectStoreReference{Name: "orders-store", State: observe.ObjectStoreUnknown}
	if check, _ := evaluateOnce(t, StoreBackupFailed{}, storeInput(unknown)); check.Outcome != CheckUnavailable {
		t.Errorf("unreadable store: %v, want could not run", check.Outcome)
	}
	none := observe.ObjectStoreReference{State: observe.ObjectStoreNotReferenced}
	if check, _ := evaluateOnce(t, StoreBackupFailed{}, storeInput(none)); check.Outcome != CheckClear {
		t.Errorf("no store referenced: %v, want clear", check.Outcome)
	}
}

// TestStoreNoBackupWaitsForTheScheduleToBeOld proves the absence of any
// successful backup is a finding only once a schedule has been around
// long enough to have fired.
func TestStoreNoBackupWaitsForTheScheduleToBeOld(t *testing.T) {
	t.Parallel()
	present := observe.ObjectStoreReference{Name: "orders-store", ServerName: "orders", State: observe.ObjectStorePresent}
	old := observe.ScheduledBackupFacts{Name: "nightly", CreatedAt: now.Add(-48 * time.Hour)}
	young := observe.ScheduledBackupFacts{Name: "hourly", CreatedAt: now.Add(-time.Hour)}
	when := StoreNoBackup{Since: 24 * time.Hour}

	check, findings := evaluateOnce(t, when, storeInput(present, young, old))
	if check.Outcome != CheckMatched || len(findings) != 1 || len(findings[0].Evidence) != 2 ||
		findings[0].Evidence[1].Object != "ScheduledBackup/nightly" {
		t.Fatalf("old schedule, no window: %v %+v", check.Outcome, findings)
	}
	if check, _ := evaluateOnce(t, when, storeInput(present, young)); check.Outcome != CheckClear {
		t.Errorf("young schedule only: %v, want clear", check.Outcome)
	}
	if check, _ := evaluateOnce(t, when, storeInput(present)); check.Outcome != CheckClear {
		t.Errorf("no schedule: %v, want clear", check.Outcome)
	}
	taken := now.Add(-time.Hour)
	present.RecoveryWindow = &observe.RecoveryWindow{LastSuccessfulBackup: &taken}
	if check, _ := evaluateOnce(t, when, storeInput(present, old)); check.Outcome != CheckClear {
		t.Errorf("a successful backup exists: %v, want clear", check.Outcome)
	}
}
