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

import "slices"

// The prior of a backup that has none (a full backup) and of one whose
// backup-prior is not in the list.
const (
	noPrior      = -1
	missingPrior = -2
)

// chainIndex is the prior structure of a backup list, built once in O(n).
// A crafted backup.info can hold a 10,000-long incremental chain: walking
// each backup's chain on its own would be quadratic, so every per-backup
// question about a whole chain (is it complete and healthy, is every
// dependency's manifest verified, is a backup an ancestor) is answered
// from this index, in one pass.
//
// A loop of priors is impossible in a file pgBackRest wrote; a backup on
// one, or whose chain reaches one, is looped and its chain incomplete.
type chainIndex struct {
	// prior is the index of each backup's prior, noPrior or missingPrior.
	prior []int
	// order lists every backup once, each after its prior, except that the
	// members of a loop come in loop order.
	order []int
	// complete: the chain reaches a backup without a prior, every prior is
	// present and there is no loop.
	complete []bool
	// loop is, for a member of a loop, the index in loops of its loop; -1
	// otherwise.
	loop  []int
	loops [][]int
	// enter and exit are each complete backup's interval in a depth-first
	// walk of the forest of complete chains: a is a proper ancestor of b
	// when a's interval strictly contains b's.
	enter, exit []int
}

func newChainIndex(reports []BackupReport, byLabel map[string]int) *chainIndex {
	n := len(reports)
	c := &chainIndex{
		prior: make([]int, n), order: make([]int, 0, n), complete: make([]bool, n), loop: make([]int, n),
		enter: make([]int, n), exit: make([]int, n),
	}
	for index, report := range reports {
		c.loop[index] = -1
		prior, ok := byLabel[report.Backup.Prior]
		switch {
		case report.Backup.Prior == "":
			c.prior[index] = noPrior
		case !ok:
			c.prior[index] = missingPrior
		default:
			c.prior[index] = prior
		}
	}
	// Each backup is resolved once: walk up the priors to a resolved
	// backup, to the end of the chain or back onto the walk itself (a
	// loop), then resolve the walk from its top down.
	const (
		unvisited = iota
		walking
		resolved
	)
	mark := make([]uint8, n)
	var walk []int
	for start := range n {
		walk = walk[:0]
		current := start
		for current >= 0 && mark[current] == unvisited {
			mark[current] = walking
			walk = append(walk, current)
			current = c.prior[current]
		}
		complete := current == noPrior || current >= 0 && mark[current] == resolved && c.complete[current]
		top := len(walk)
		if current >= 0 && mark[current] == walking {
			// The walk closed a loop from current to its end.
			top = slices.Index(walk, current)
			members := slices.Clone(walk[top:])
			for _, member := range members {
				c.loop[member] = len(c.loops)
				mark[member] = resolved
			}
			c.loops = append(c.loops, members)
			c.order = append(c.order, members...)
		}
		for position := top - 1; position >= 0; position-- {
			node := walk[position]
			c.complete[node], mark[node] = complete, resolved
			c.order = append(c.order, node)
		}
	}
	c.intervals()
	return c
}

// intervals numbers the complete backups by a depth-first walk from the
// backups without a prior.
func (c *chainIndex) intervals() {
	children := make([][]int, len(c.prior))
	var stack []int
	for _, index := range c.order {
		switch prior := c.prior[index]; {
		case !c.complete[index]:
		case prior == noPrior:
			stack = append(stack, index)
		default:
			children[prior] = append(children[prior], index)
		}
	}
	clock := 0
	// A node is pushed as itself to enter it and as its complement to
	// leave it once its children are done.
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node < 0 {
			c.exit[^node] = clock
			clock++
			continue
		}
		c.enter[node] = clock
		clock++
		stack = append(stack, ^node)
		stack = append(stack, children[node]...)
	}
}

// isAncestor reports whether ancestor is in index's chain of priors (not
// index itself); it is only decided for a complete chain and false
// otherwise.
func (c *chainIndex) isAncestor(ancestor, index int) bool {
	return c.complete[index] && c.complete[ancestor] &&
		c.enter[ancestor] < c.enter[index] && c.exit[index] < c.exit[ancestor]
}

// healthy is, for every backup, whether its chain is complete and every
// backup of it, itself included, passes ownOK.
func (c *chainIndex) healthy(ownOK []bool) []bool {
	out := make([]bool, len(c.prior))
	for _, index := range c.order {
		switch prior := c.prior[index]; {
		case !c.complete[index]:
		case prior == noPrior:
			out[index] = ownOK[index]
		default:
			out[index] = ownOK[index] && out[prior]
		}
	}
	return out
}

// foldDependencies computes, for every backup, the fold of add over its
// dependencies (the backups of its chain of priors, itself excluded),
// from zero: add(add(zero, prior), prior's prior)... add must not depend
// on the order it is applied in (a minimum, a worst state, a conjunction).
// A member of a loop folds every member of the loop, itself included
// (conservative: pgBackRest never writes a loop).
func foldDependencies[T any](c *chainIndex, zero T, add func(T, int) T) []T {
	out := make([]T, len(c.prior))
	loops := make([]T, len(c.loops))
	for number, members := range c.loops {
		value := zero
		for _, member := range members {
			value = add(value, member)
		}
		loops[number] = value
	}
	for _, index := range c.order {
		switch prior := c.prior[index]; {
		case c.loop[index] >= 0:
			out[index] = loops[c.loop[index]]
		case prior < 0:
			out[index] = zero
		default:
			out[index] = add(out[prior], prior)
		}
	}
	return out
}

// dependencies returns the backups index needs to restore: its chain of
// priors, sorted, without index itself. It walks one chain; checkManifests
// asks it for a bounded number of backups only.
func (c *chainIndex) dependencies(index int) []int {
	result := make([]int, 0, 4)
	if c.loop[index] >= 0 {
		for _, member := range c.loops[c.loop[index]] {
			if member != index {
				result = append(result, member)
			}
		}
		slices.Sort(result)
		return result
	}
	for current := c.prior[index]; current >= 0; current = c.prior[current] {
		if c.loop[current] >= 0 {
			result = append(result, c.loops[c.loop[current]]...)
			break
		}
		result = append(result, current)
	}
	slices.Sort(result)
	return result
}
