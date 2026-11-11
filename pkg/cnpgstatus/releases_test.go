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

package cnpgstatus

// verifiedRelease is one released tag a minor's tables are checked at.
// election: the tag has the 1.30.1 election rules the failover rehearsal
// follows (fenced instances ranked last, no election when all are
// fenced, the quorum checked again before promoting), which 1.30.0 does
// not (TestElectionRulesAreAbsentAt1300).
type verifiedRelease struct {
	minor    Minor
	tag      string
	election bool
}

// verifiedReleases are every supported release the tables are checked
// at (pkg/diagnose/catalog/cnpg VerifiedReleases is the same list): a
// literal that 1.30.0 lacks fails here as it does at 1.30.1. 1.31 is
// planned (upstream main) and has no release to fetch.
var verifiedReleases = []verifiedRelease{
	{minor: Minor130, tag: "1.30.0"},
	{minor: Minor130, tag: "1.30.1", election: true},
}

// VerifiedReleaseTags are verifiedReleases' tags, oldest first, for the
// external test that checks them against the catalog's list
// (releases_external_test.go: this package cannot import the catalog,
// which imports it).
func VerifiedReleaseTags() []string {
	out := make([]string, 0, len(verifiedReleases))
	for _, v := range verifiedReleases {
		out = append(out, v.tag)
	}
	return out
}
