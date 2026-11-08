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

package pgbackrest

import (
	"fmt"
	"slices"
	"testing"
)

// chainReports builds a backup list from (label, prior) pairs.
func chainReports(pairs ...[2]string) ([]BackupReport, map[string]int) {
	reports := make([]BackupReport, len(pairs))
	byLabel := make(map[string]int, len(pairs))
	for index, pair := range pairs {
		reports[index] = BackupReport{Backup: Backup{Label: pair[0], Prior: pair[1]}}
		byLabel[pair[0]] = index
	}
	return reports, byLabel
}

// walkChain is the reference definition: index's priors, oldest last, with
// -1 for a missing prior or a loop.
func walkChain(reports []BackupReport, byLabel map[string]int, index int) []int {
	chain := []int{index}
	seen := map[int]bool{index: true}
	for current := index; reports[current].Backup.Prior != ""; {
		next, ok := byLabel[reports[current].Backup.Prior]
		if !ok || seen[next] {
			return append(chain, -1)
		}
		chain, seen[next], current = append(chain, next), true, next
	}
	return chain
}

func TestChainIndexMatchesAWalkPerBackup(t *testing.T) {
	t.Parallel()
	cases := map[string][][2]string{
		"one chain":           {{"F", ""}, {"D1", "F"}, {"I1", "D1"}, {"I2", "I1"}},
		"two fulls":           {{"F1", ""}, {"F2", ""}, {"I1", "F1"}, {"I2", "F2"}, {"I3", "I1"}},
		"a missing prior":     {{"F", ""}, {"I1", "gone"}, {"I2", "I1"}},
		"a loop and its tail": {{"A", "B"}, {"B", "C"}, {"C", "A"}, {"T1", "B"}, {"T2", "T1"}, {"F", ""}},
		"a self loop":         {{"A", "A"}, {"T", "A"}},
		"children before":     {{"I2", "I1"}, {"I1", "F"}, {"F", ""}},
	}
	for name, pairs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reports, byLabel := chainReports(pairs...)
			chains := newChainIndex(reports, byLabel)
			ok := make([]bool, len(reports))
			for index := range ok {
				ok[index] = true
			}
			healthy := chains.healthy(ok)
			for index := range reports {
				chain := walkChain(reports, byLabel, index)
				complete := !slices.Contains(chain, -1)
				if chains.complete[index] != complete || healthy[index] != complete {
					t.Errorf("%s: complete %v healthy %v, want %v", reports[index].Backup.Label, chains.complete[index], healthy[index], complete)
				}
				var want []int
				for _, ancestor := range chain[1:] {
					if ancestor >= 0 {
						want = append(want, ancestor)
					}
				}
				slices.Sort(want)
				if got := chains.dependencies(index); !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
					t.Errorf("%s: dependencies %v, want %v", reports[index].Backup.Label, got, want)
				}
				for other := range reports {
					want := complete && slices.Contains(chain[1:], other)
					if got := chains.isAncestor(other, index); got != want {
						t.Errorf("isAncestor(%s, %s) = %v, want %v", reports[other].Backup.Label, reports[index].Backup.Label, got, want)
					}
				}
			}
		})
	}
}

func TestChainIndexHealthFollowsABrokenLink(t *testing.T) {
	t.Parallel()
	reports, byLabel := chainReports([2]string{"F", ""}, [2]string{"I1", "F"}, [2]string{"I2", "I1"}, [2]string{"I3", "I2"})
	chains := newChainIndex(reports, byLabel)
	healthy := chains.healthy([]bool{true, false, true, true})
	if !slices.Equal(healthy, []bool{true, false, false, false}) {
		t.Fatalf("healthy %v: a broken link breaks every backup after it, and only those", healthy)
	}
	minimum := foldDependencies(chains, len(reports), func(acc, dependency int) int { return min(acc, dependency) })
	if !slices.Equal(minimum, []int{4, 0, 0, 0}) {
		t.Fatalf("fold %v", minimum)
	}
}

// TestChainIndexIsLinear: a 10,000-long incremental chain (MaxBackups) is
// indexed and asked every per-backup question in a number of steps
// proportional to its length, counted through the fold: one step per
// backup, not one per (backup, ancestor) pair.
func TestChainIndexIsLinear(t *testing.T) {
	t.Parallel()
	pairs := make([][2]string, MaxBackups)
	pairs[0] = [2]string{"F", ""}
	for index := 1; index < MaxBackups; index++ {
		pairs[index] = [2]string{fmt.Sprintf("I%05d", index), pairs[index-1][0]}
	}
	// Newest first, as a crafted file may order them.
	slices.Reverse(pairs)
	reports, byLabel := chainReports(pairs...)
	chains := newChainIndex(reports, byLabel)
	steps := 0
	depth := foldDependencies(chains, 0, func(acc, _ int) int { steps++; return acc + 1 })
	if steps != MaxBackups-1 {
		t.Fatalf("the fold took %d steps for %d backups", steps, MaxBackups)
	}
	if depth[0] != MaxBackups-1 || !chains.complete[0] || !chains.isAncestor(len(reports)-1, 0) || chains.isAncestor(0, len(reports)-1) {
		t.Fatalf("depth %d, complete %v", depth[0], chains.complete[0])
	}
	ok := make([]bool, len(reports))
	for index := range ok {
		ok[index] = true
	}
	ok[len(reports)/2] = false
	healthy := chains.healthy(ok)
	if healthy[len(reports)-1] != true || healthy[0] != false {
		t.Fatalf("health of the oldest %v and newest %v", healthy[len(reports)-1], healthy[0])
	}
}

// BenchmarkAssessALongChain: Assess on a 10,000-long chain is linear.
func BenchmarkAssessALongChain(b *testing.B) {
	backups := make([]BackupReport, MaxBackups)
	backups[0] = BackupReport{Backup: Backup{Label: "F", Type: BackupFull}}
	for index := 1; index < MaxBackups; index++ {
		backups[index] = BackupReport{Backup: Backup{Label: fmt.Sprintf("I%05d", index), Type: BackupIncremental, Prior: backups[index-1].Backup.Label}}
	}
	report := Report{Backups: backups}
	for b.Loop() {
		_ = report.Assess()
	}
}
