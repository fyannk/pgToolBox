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

// Package cnpgstatus is the per-minor CloudNativePG read model: which
// phase strings and condition types each supported operator minor
// writes, what they mean for a DBA, and the facts the console derives
// from a Cluster and its Pods (version, fencing, hibernation, escape
// hatches, backups, archiving, primary, status-port transport, blind
// spots).
//
// Every table entry names the operator source file and the tag it was
// read at. Facts about 1.31 describe upstream main on 2026-10-23
// (commit 21e24f6) and are planned, not released: re-check them when
// 1.31.0 is tagged. Nothing here talks to a cluster; the functions are
// pure over the typed objects the console already holds.
package cnpgstatus

// Class is the console's status vocabulary. It is richer than the
// green/yellow/red of kubectl cnpg status: "progressing" separates a
// transition from a fault, "inactive" a deliberate stop (hibernation,
// fencing) from a failure, and "unknown" is never painted healthy.
type Class string

// The classes, in no particular order; Worst ranks them.
const (
	ClassOK          Class = "ok"
	ClassProgressing Class = "progressing"
	ClassWarning     Class = "warning"
	ClassCritical    Class = "critical"
	ClassInactive    Class = "inactive"
	ClassUnknown     Class = "unknown"
	ClassInfo        Class = "info"
)

// classRank orders the classes for roll-ups. Unknown ranks above every
// healthy-looking class so that a missing fact can never hide behind an
// "ok" one, and below the two that state a known problem.
var classRank = map[Class]int{
	ClassInfo:        0,
	ClassOK:          1,
	ClassInactive:    2,
	ClassProgressing: 3,
	ClassUnknown:     4,
	ClassWarning:     5,
	ClassCritical:    6,
}

// Worst returns the most severe class of its arguments, ranked
// info < ok < inactive < progressing < unknown < warning < critical. No
// argument, or an argument that is not one of the seven classes, counts
// as unknown: a roll-up of nothing is not healthy.
func Worst(classes ...Class) Class {
	if len(classes) == 0 {
		return ClassUnknown
	}
	worst := ClassInfo
	for _, class := range classes {
		if _, known := classRank[class]; !known {
			class = ClassUnknown
		}
		if classRank[class] > classRank[worst] {
			worst = class
		}
	}
	return worst
}

// Valid reports whether c is one of the seven classes.
func (c Class) Valid() bool {
	_, ok := classRank[c]
	return ok
}
