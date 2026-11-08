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

package insight

import "sort"

// BlockNode is one backend in the lock graph with the backends waiting
// on it.
type BlockNode struct {
	Session Session
	// Missing marks a blocker the sample does not include (the pack keeps
	// 50 rows, root blockers first): only its pid is known.
	Missing bool
	// Cycle marks a backend met again below itself: a lock cycle the
	// deadlock detector has not broken yet (it runs after
	// deadlock_timeout).
	Cycle bool
	// Waiters wait on this backend directly.
	Waiters []BlockNode
	// Blocked counts every backend below this one.
	Blocked int
}

// maxDepth bounds the tree walk; a lock queue deeper than this is drawn
// cut, never followed without end.
const maxDepth = 32

// BlockingTree builds the lock graph of one instance's sessions: root
// blockers first (those that wait on nobody in the graph), the most
// backends blocked first, then the oldest transaction. A waiter whose
// blocker is not in the sample hangs under a Missing node for that pid.
// A backend waiting on several blockers appears under each.
func BlockingTree(sessions []Session) []BlockNode {
	byPID := make(map[int64]Session, len(sessions))
	waitersOf := map[int64][]int64{}
	for _, s := range sessions {
		byPID[s.PID] = s
	}
	missing := map[int64]bool{}
	for _, s := range sessions {
		for _, b := range s.BlockedBy {
			waitersOf[b] = append(waitersOf[b], s.PID)
			if _, ok := byPID[b]; !ok {
				missing[b] = true
			}
		}
	}
	for pid := range waitersOf {
		sort.Slice(waitersOf[pid], func(i, j int) bool { return waitersOf[pid][i] < waitersOf[pid][j] })
	}
	var build func(pid int64, path map[int64]bool, depth int) BlockNode
	build = func(pid int64, path map[int64]bool, depth int) BlockNode {
		node := BlockNode{Session: byPID[pid]}
		if missing[pid] {
			node.Session, node.Missing = Session{PID: pid}, true
		}
		if path[pid] {
			node.Cycle = true
			return node
		}
		if depth >= maxDepth {
			return node
		}
		path[pid] = true
		for _, w := range waitersOf[pid] {
			child := build(w, path, depth+1)
			node.Blocked += 1 + child.Blocked
			node.Waiters = append(node.Waiters, child)
		}
		delete(path, pid)
		sortNodes(node.Waiters)
		return node
	}
	var roots []BlockNode
	for _, s := range sessions {
		if len(s.BlockedBy) == 0 && len(waitersOf[s.PID]) > 0 {
			roots = append(roots, build(s.PID, map[int64]bool{}, 0))
		}
	}
	for pid := range missing {
		roots = append(roots, build(pid, map[int64]bool{}, 0))
	}
	// A cycle has no root: every member waits on another. Draw it from
	// its lowest pid so it is not lost.
	drawn := map[int64]bool{}
	var mark func(n BlockNode)
	mark = func(n BlockNode) {
		drawn[n.Session.PID] = true
		for _, w := range n.Waiters {
			mark(w)
		}
	}
	for _, r := range roots {
		mark(r)
	}
	pids := make([]int64, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, pid := range pids {
		if !drawn[pid] && len(waitersOf[pid]) > 0 {
			root := build(pid, map[int64]bool{}, 0)
			mark(root)
			roots = append(roots, root)
		}
	}
	sortNodes(roots)
	return roots
}

func sortNodes(nodes []BlockNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.Blocked != b.Blocked {
			return a.Blocked > b.Blocked
		}
		ax, bx := value(a.Session.XactAgeSeconds), value(b.Session.XactAgeSeconds)
		if ax != bx {
			return ax > bx
		}
		return a.Session.PID < b.Session.PID
	})
}

func value(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}
