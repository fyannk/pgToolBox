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

package cnpg

import (
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// TestBacklogFindingsFireAfterTheRecoveryScreens: the archive-backlog
// findings (a high-severity alarm) fire at a count the recovery screens
// and the archiving verdict already read as a backlog
// (cnpgstatus.ArchiveBacklogSegments), so the two never contradict: a
// screen with the finding never reads healthy archiving.
func TestBacklogFindingsFireAfterTheRecoveryScreens(t *testing.T) {
	for name, threshold := range map[string]int{"cnpg-wal-archive-backlog": archiveBacklogSegments, "instance readyWalFiles": readyWALFiles} {
		if threshold < cnpgstatus.ArchiveBacklogSegments {
			t.Errorf("%s fires at %d, below the archiving verdict's backlog at %d", name, threshold, cnpgstatus.ArchiveBacklogSegments)
		}
	}
}
