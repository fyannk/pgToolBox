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
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The evidence agent writes its timeline reasons in English from a small
// set of templates (internal/evidence/formats/timeline: Gap, OwnReason,
// BeyondReason). The console does not change the wire: it recognises
// those shapes and says them as Phrases a client translates, keeping
// the agent's text as the fallback. A message of any other shape, or one
// the agent cut at its bound, is shown as written.
var (
	gapUnreadable = `history (\d{1,10}) is unreadable or invalid`
	gapNotAllRead = `timeline history files were not all read`
	gapMissing    = `history (\d{1,10}) is missing`
	gapAny        = `(?:` + gapUnreadable + `|` + gapNotAllRead + `|` + gapMissing + `)`

	reHistoryMissingNoFollow = regexp.MustCompile(`^history (\d{1,10}) is missing: no restore can follow timeline (\d{1,10})$`)
	reNeedsTarget            = regexp.MustCompile(`^timeline (\d{1,10}) needs an explicit targetTLI; ` + gapAny + `$`)
	reLatestUnknown          = regexp.MustCompile(`^the timeline a default restore follows is unknown: ` + gapAny + `$`)
	reLatestStops            = regexp.MustCompile(`^a default restore \(recovery_target_timeline 'latest'\) stops on timeline (\d{1,10}): ` + gapAny + `; timeline (\d{1,10}) needs an explicit targetTLI$`)

	// reHistoryFile finds a history file named in any reason: by its
	// file name, or as "history N is missing / unreadable".
	reHistoryFileName = regexp.MustCompile(`\b([0-9A-Fa-f]{8})\.history\b`)
	reHistoryNumber   = regexp.MustCompile(`\bhistory (\d{1,10}) is (?:missing|unreadable)`)
)

// gapParams are the parameters of the gap groups (unreadable, not all
// read, missing) of a match whose gap groups start at index i.
func gapParams(m []string, i int, p map[string]string) {
	switch {
	case m[i] != "":
		p["gap"], p["gapTimeline"] = "unreadable", m[i]
	case m[i+1] != "":
		p["gap"], p["gapTimeline"] = "missing", m[i+1]
	default:
		p["gap"] = "notAllRead"
	}
	if f := historyFileOf(p["gapTimeline"]); f != "" {
		p["historyFile"] = f
	}
}

// timelinePhrase says a timeline reason of the agent's as a Phrase; false
// when its shape is not one of the templates.
func timelinePhrase(msg string) (viewmodel.Phrase, bool) {
	p := map[string]string{}
	switch {
	case reHistoryMissingNoFollow.MatchString(msg):
		m := reHistoryMissingNoFollow.FindStringSubmatch(msg)
		if m[1] != m[2] {
			return viewmodel.Phrase{}, false
		}
		p["timeline"], p["historyFile"] = m[1], historyFileOf(m[1])
		return say("timeline.reason.historyMissingNoFollow", msg, p), true
	case reNeedsTarget.MatchString(msg):
		m := reNeedsTarget.FindStringSubmatch(msg)
		p["timeline"] = m[1]
		gapParams(m, 2, p)
		return say("timeline.reason.needsTarget", msg, p), true
	case reLatestUnknown.MatchString(msg):
		gapParams(reLatestUnknown.FindStringSubmatch(msg), 1, p)
		return say("timeline.reason.latestUnknown", msg, p), true
	case reLatestStops.MatchString(msg):
		m := reLatestStops.FindStringSubmatch(msg)
		p["latest"], p["newest"] = m[1], m[4]
		gapParams(m, 2, p)
		return say("timeline.reason.latestStops", msg, p), true
	}
	return viewmodel.Phrase{}, false
}

// evidenceReason says a reason the evidence wrote: a timeline template
// when it is one, else code with the text as its detail.
func evidenceReason(code, msg string) viewmodel.Phrase {
	if p, ok := timelinePhrase(msg); ok {
		return p
	}
	return say(code, msg, map[string]string{"detail": msg})
}

// historyFileOf is timeline n's history file name (00000003.history);
// empty when n is not a timeline.
func historyFileOf(n string) string {
	v, err := strconv.ParseUint(n, 10, 32)
	if err != nil || v == 0 {
		return ""
	}
	return fmt.Sprintf("%08X.history", v)
}

// historyFileIn is the first timeline history file a reason names; empty
// when none.
func historyFileIn(texts ...string) string {
	for _, t := range texts {
		if m := reHistoryFileName.FindStringSubmatch(t); m != nil {
			return strings.ToUpper(m[1]) + ".history"
		}
		if m := reHistoryNumber.FindStringSubmatch(t); m != nil {
			if f := historyFileOf(m[1]); f != "" {
				return f
			}
		}
	}
	return ""
}
