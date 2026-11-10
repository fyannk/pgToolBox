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
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The agent's timeline reasons arrive over the wire from the evidence
// agent: untrusted text. The fuzz targets assert what the SPA relies on,
// not only that nothing panics: a recognised reason keeps its text, its
// code is one the SPA translates, every enumerated parameter is from its
// known set, every free parameter is taken from the input, and a history
// file named is a well-formed name the input names.

var timelineReasonSeeds = []string{
	"history 3 is missing: no restore can follow timeline 3",
	"timeline 12 needs an explicit targetTLI; history 11 is unreadable or invalid",
	"the timeline a default restore follows is unknown: history 4 is unreadable",
	"the timeline a default restore follows is unknown: timeline history files were not all read",
	"a default restore follows timeline 3, whose history does not lead through timeline 2: needs targetTLI 2",
	"timeline ancestry contains missing or invalid evidence; 00000003.history: timeline history is malformed",
	"00000002.history: timeline history is malformed; no default restore reads it",
	"a default restore (recovery_target_timeline 'latest') stops on timeline 2: history 3 is missing; timeline 4 needs an explicit targetTLI",
	"the agent's timeline evidence is unknown (timeline ancestry contains missing or invalid evidence; 00000005.history: timeline history could not be read): no recovery path is shown as coverage until the timeline a default restore follows is established",
	"the agent's timeline evidence is warning (history 0 is missing): no recovery path is shown as coverage until the timeline a default restore follows is established",
	agentPredatesReason,
	"timeline history 0000000a.history could not be read",
	"history 4294967296 is missing: no restore can follow timeline 4294967296",
	"",
}

var (
	knownTimelineCodes = map[string]bool{
		"recovery.reason.agentPredates": true, "timeline.reason.historyMissingNoFollow": true,
		"timeline.reason.needsTarget": true, "timeline.reason.latestUnknown": true, "timeline.reason.latestStops": true,
		"timeline.reason.needsTargetSibling": true, "timeline.reason.ancestryInvalid": true,
		"timeline.reason.unneededInvalid": true, "recovery.reason.timelineNotEstablished": true,
	}
	enumParams = map[string]map[string]bool{
		"gap":     {"invalid": true, "unreadable": true, "missing": true, "notAllRead": true},
		"problem": {"duplicate": true, "unreadable": true, "malformed": true, "contradictory": true, "cyclic": true, "rangeDisagrees": true},
		"state":   {"unknown": true, "unhealthy": true, "warning": true},
	}
	numericParams = map[string]bool{"timeline": true, "gapTimeline": true, "latest": true, "newest": true}
	historyName   = regexp.MustCompile(`^[0-9A-F]{8}\.history$`)
)

// namedBy reports a history file the text names: by its file name in any
// case, or by its timeline number in decimal.
func namedBy(file, text string) bool {
	if !historyName.MatchString(file) {
		return false
	}
	if strings.Contains(strings.ToUpper(text), strings.ToUpper(file)) {
		return true
	}
	v, err := strconv.ParseUint(strings.TrimSuffix(file, ".history"), 16, 32)
	return err == nil && v > 0 && strings.Contains(text, strconv.FormatUint(v, 10))
}

func FuzzTimelinePhrase(f *testing.F) {
	for _, s := range timelineReasonSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg string) {
		p, ok := timelinePhrase(msg)
		if !ok {
			if p.Code != "" || p.Text != "" || len(p.Params) != 0 {
				t.Fatalf("unrecognised %q returned %+v", msg, p)
			}
			return
		}
		if p.Text != msg {
			t.Fatalf("text %q is not the input %q", p.Text, msg)
		}
		if !knownTimelineCodes[p.Code] {
			t.Fatalf("code %q is not one the SPA translates", p.Code)
		}
		for k, v := range p.Params {
			switch {
			case k == "historyFile":
				if !namedBy(v, msg) {
					t.Fatalf("history file %q is not named by %q", v, msg)
				}
			case k == "inner":
				if !knownTimelineCodes[v] {
					t.Fatalf("inner code %q unknown", v)
				}
			case enumParams[k] != nil:
				if !enumParams[k][v] {
					t.Fatalf("%s = %q is not in its set", k, v)
				}
			case numericParams[k]:
				if _, err := strconv.ParseUint(v, 10, 64); err != nil || !strings.Contains(msg, v) {
					t.Fatalf("%s = %q is not a number of the input", k, v)
				}
			case k == "detail":
				if !strings.Contains(msg, v) {
					t.Fatalf("detail %q is not part of the input", v)
				}
			default:
				t.Fatalf("unexpected parameter %s = %q", k, v)
			}
		}
	})
}

func FuzzHistoryFileIn(f *testing.F) {
	for _, s := range timelineReasonSeeds {
		f.Add(s, "")
	}
	f.Add("no history named", "history 7 is unreadable")
	f.Fuzz(func(t *testing.T, a, b string) {
		got := historyFileIn(a, b)
		if got == "" {
			return
		}
		if !namedBy(got, a) && !namedBy(got, b) {
			t.Fatalf("historyFileIn(%q, %q) = %q, named by neither", a, b, got)
		}
	})
}
