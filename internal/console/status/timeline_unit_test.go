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
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// TestJournalEntryCarriesUncertainty: a primary change whose cause the
// console did not see reaches the API marked uncertain, drawn as a
// warning, never as a failover.
func TestJournalEntryCarriesUncertainty(t *testing.T) {
	e := &timeline.Event{Seq: 1, Kind: timeline.KindPrimary, Action: timeline.ActionChanged, Uncertain: true, Namespace: "a", Cluster: "orders", From: "orders-1", To: "orders-2"}
	got := journalEntry(e, "")
	if !got.Uncertain || got.Action != timeline.ActionChanged || got.Health != viewmodel.HealthWarning {
		t.Fatalf("got %+v", got)
	}
	e.Uncertain = false
	if journalEntry(e, "").Uncertain {
		t.Fatal("uncertain set on a certain change")
	}
}
