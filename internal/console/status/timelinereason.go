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
// BeyondReason, NeedsReason; barmancloud narrowHistories), and the
// console's evidence ingest adds its own (internal/console/evidence
// cap.go). The console does not change the wire: it recognises those
// shapes and says them as Phrases a client translates, keeping the text
// as the fallback. A message of any other shape, or one cut at its bound,
// is shown as written.
var (
	gapInvalid    = `history (\d{1,10}) is unreadable or invalid`
	gapUnreadable = `history (\d{1,10}) is unreadable`
	gapNotAllRead = `timeline history files were not all read`
	gapMissing    = `history (\d{1,10}) is missing`
	gapAny        = `(?:` + gapInvalid + `|` + gapUnreadable + `|` + gapNotAllRead + `|` + gapMissing + `)`

	// historyProblems are the reasons barmancloud gives a history row it
	// could not use, by the code a client translates.
	historyProblems = map[string]string{
		"duplicate timeline history objects":                  "duplicate",
		"timeline history could not be read":                  "unreadable",
		"timeline history is malformed":                       "malformed",
		"timeline histories contradict one another":           "contradictory",
		"timeline ancestry is cyclic":                         "cyclic",
		"child WAL range disagrees with timeline switchpoint": "rangeDisagrees",
	}
	historyRow = `([0-9A-Fa-f]{8}\.history): ([a-zA-Z ]{1,80})`

	reHistoryMissingNoFollow = regexp.MustCompile(`^history (\d{1,10}) is missing: no restore can follow timeline (\d{1,10})$`)
	reNeedsTarget            = regexp.MustCompile(`^timeline (\d{1,10}) needs an explicit targetTLI; ` + gapAny + `$`)
	reLatestUnknown          = regexp.MustCompile(`^the timeline a default restore follows is unknown: ` + gapAny + `$`)
	reLatestStops            = regexp.MustCompile(`^a default restore \(recovery_target_timeline 'latest'\) stops on timeline (\d{1,10}): ` + gapAny + `; timeline (\d{1,10}) needs an explicit targetTLI$`)
	reNeedsSibling           = regexp.MustCompile(`^a default restore follows timeline (\d{1,10}), whose history does not lead through timeline (\d{1,10}): needs targetTLI (\d{1,10})$`)
	reAncestryInvalid        = regexp.MustCompile(`^timeline ancestry contains missing or invalid evidence(?:; ` + historyRow + `)?$`)
	reUnneededInvalid        = regexp.MustCompile(`^` + historyRow + `; no default restore reads it$`)
	reNotEstablished         = regexp.MustCompile(`^the agent's timeline evidence is (unknown|unhealthy|warning)(?: \((.+)\))?: no recovery path is shown as coverage until the timeline a default restore follows is established$`)

	// reHistoryFile finds a history file named in any reason: by its
	// file name, or as "history N is missing / unreadable".
	reHistoryFileName = regexp.MustCompile(`\b([0-9A-Fa-f]{8})\.history\b`)
	reHistoryNumber   = regexp.MustCompile(`\bhistory (\d{1,10}) is (?:missing|unreadable)`)
)

// agentPredatesReason is cap.go's reason for a producer without the
// timeline features the console requires (reasonProducerPredatesTimelineLatest).
const agentPredatesReason = "the evidence agent is an older build that does not resolve recovery_target_timeline 'latest' " +
	"as PostgreSQL does: its recovery paths are not shown as coverage; upgrade the agent"

// gapParams are the parameters of the gap groups (invalid, unreadable,
// missing; not all read when none matched) of a match whose gap groups
// start at index i.
func gapParams(m []string, i int, p map[string]string) {
	switch {
	case m[i] != "":
		p["gap"], p["gapTimeline"] = "invalid", m[i]
	case m[i+1] != "":
		p["gap"], p["gapTimeline"] = "unreadable", m[i+1]
	case m[i+2] != "":
		p["gap"], p["gapTimeline"] = "missing", m[i+2]
	default:
		p["gap"] = "notAllRead"
	}
	if f := historyFileOf(p["gapTimeline"]); f != "" {
		p["historyFile"] = f
	}
}

// historyRowParams reads a "<file>: <problem>" group pair; false when the
// problem is not one barmancloud writes.
func historyRowParams(file, problem string, p map[string]string) bool {
	code, ok := historyProblems[problem]
	if !ok {
		return false
	}
	p["historyFile"], p["problem"] = strings.ToUpper(strings.TrimSuffix(file, ".history"))+".history", code
	return true
}

// timelinePhrase says a timeline reason as a Phrase; false when its shape
// is not one of the templates.
func timelinePhrase(msg string) (viewmodel.Phrase, bool) {
	p := map[string]string{}
	switch {
	case msg == agentPredatesReason:
		return say("recovery.reason.agentPredates", msg, nil), true
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
		p["latest"], p["newest"] = m[1], m[5]
		gapParams(m, 2, p)
		return say("timeline.reason.latestStops", msg, p), true
	case reNeedsSibling.MatchString(msg):
		m := reNeedsSibling.FindStringSubmatch(msg)
		if m[2] != m[3] {
			return viewmodel.Phrase{}, false
		}
		p["latest"], p["timeline"] = m[1], m[2]
		return say("timeline.reason.needsTargetSibling", msg, p), true
	case reAncestryInvalid.MatchString(msg):
		m := reAncestryInvalid.FindStringSubmatch(msg)
		if m[1] != "" && !historyRowParams(m[1], m[2], p) {
			return viewmodel.Phrase{}, false
		}
		return say("timeline.reason.ancestryInvalid", msg, p), true
	case reUnneededInvalid.MatchString(msg):
		m := reUnneededInvalid.FindStringSubmatch(msg)
		if !historyRowParams(m[1], m[2], p) {
			return viewmodel.Phrase{}, false
		}
		return say("timeline.reason.unneededInvalid", msg, p), true
	case reNotEstablished.MatchString(msg):
		m := reNotEstablished.FindStringSubmatch(msg)
		if m[2] != "" {
			// The agent's own timeline reason inside cap.go's: said too
			// when it is a template (its code under "inner").
			if inner, ok := timelinePhrase(m[2]); ok {
				for k, v := range inner.Params {
					p[k] = v
				}
				p["inner"] = inner.Code
			}
			p["detail"] = m[2]
			if f := historyFileIn(m[2]); f != "" && p["historyFile"] == "" {
				p["historyFile"] = f
			}
		}
		p["state"] = m[1]
		return say("recovery.reason.timelineNotEstablished", msg, p), true
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
