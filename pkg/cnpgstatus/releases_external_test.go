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

package cnpgstatus_test

import (
	"slices"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// TestVerifiedReleasesMatchTheCatalog (round 10, CNPG idea): the
// releases the phase and condition tables are checked at (make
// verify-pins-cnpgstatus) are the releases the diagnostics catalog's
// strings were read from (make verify-pins). A release added to one list
// only would leave the other's facts unverified at it.
func TestVerifiedReleasesMatchTheCatalog(t *testing.T) {
	t.Parallel()
	if got, want := cnpgstatus.VerifiedReleaseTags(), cnpg.VerifiedReleases; !slices.Equal(got, want) {
		t.Fatalf("cnpgstatus verifies the tables at %v, the catalog its strings at %v: the lists must be the same", got, want)
	}
}
