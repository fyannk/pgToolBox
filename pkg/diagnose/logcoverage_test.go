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

package diagnose

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
)

// coveredLogs is a log read side that reports both what matched and
// which containers are going unheard.
type coveredLogs struct {
	observed []logstream.Observation
	unread   []logstream.Unread
}

func (l coveredLogs) Observations() []logstream.Observation { return l.observed }

func (l coveredLogs) Unread() []logstream.Unread { return l.unread }

// coverageRule is a synthetic log-backed rule, the shape 27 catalog rules
// share.
var coverageRule = Rule{
	ID: "log-check", Summary: "A line appeared.",
	When: LogContains{Substrings: []string{"boom"}},
}

// blind is one container the follower is not reading, gone for the given
// span.
func blind(pod string, ago time.Duration, reason string) logstream.Unread {
	return logstream.Unread{Pod: pod, Container: "postgres", Since: now.Add(-ago), Reason: reason}
}

// TestALogCheckWillNotClearWhileAContainerGoesUnheard is the honesty
// invariant on the source where absence is the entire signal. A log rule
// fires on a line appearing, so its clear result is the claim that the
// line was not written — and that claim rests wholly on the console
// having been listening. While a stream is not open it was not, so the
// check has to withdraw rather than report that it looked.
func TestALogCheckWillNotClearWhileAContainerGoesUnheard(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, HasPods: true, Logs: coveredLogs{unread: []logstream.Unread{
		blind("orders-1", 4*time.Minute, "stream could not be opened"),
	}}}
	check, findings := evaluateRule(coverageRule, in)
	if check.Outcome != CheckUnavailable || len(findings) != 0 {
		t.Fatalf("outcome = %v with %d findings, want could-not-run", check.Outcome, len(findings))
	}
	for _, want := range []string{"orders-1", "postgres", "4m0s", "stream could not be opened"} {
		if !strings.Contains(check.Because, want) {
			t.Errorf("reason = %q, want it to carry %q", check.Because, want)
		}
	}
}

// TestALogCheckClearsWhenEveryContainerIsBeingRead is the other half:
// the bound must not be so broad that a healthy follower never lets a
// check say it looked, which would make the whole log catalog useless.
func TestALogCheckClearsWhenEveryContainerIsBeingRead(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, HasPods: true, Logs: coveredLogs{}}
	check, _ := evaluateRule(coverageRule, in)
	if check.Outcome != CheckClear {
		t.Errorf("outcome with every stream open = %v (%s), want clear", check.Outcome, check.Because)
	}
}

// TestALogMatchStandsWhateverElseWentUnheard keeps the rule from cutting
// the wrong way. A line that was seen was seen: an unread container
// elsewhere is a reason not to conclude silence, never a reason to
// withdraw a finding the console actually has.
func TestALogMatchStandsWhateverElseWentUnheard(t *testing.T) {
	t.Parallel()
	seen := now.Add(-2 * time.Minute)
	in := Input{Now: now, HasPods: true, Logs: coveredLogs{
		observed: []logstream.Observation{{
			RuleID: "log-check", Pod: "orders-1", Container: "postgres",
			Line: "boom", FirstSeen: seen, LastSeen: seen, Count: 1,
		}},
		unread: []logstream.Unread{blind("orders-2", time.Minute, "stream ended")},
	}}
	check, findings := evaluateRule(coverageRule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with %d findings (%s), want the match to stand",
			check.Outcome, len(findings), check.Because)
	}
	if findings[0].Subject.Name != "orders-1" {
		t.Errorf("subject = %s, want the container the line came from", findings[0].Subject)
	}
}

// TestFollowingBeingOffOutranksCoverage pins the order of the two
// reasons. With following off there is no follower to be behind, and
// "log following is off" is what an operator can act on.
func TestFollowingBeingOffOutranksCoverage(t *testing.T) {
	t.Parallel()
	check, _ := evaluateRule(coverageRule, Input{Now: now})
	if check.Outcome != CheckUnavailable {
		t.Fatalf("outcome = %v, want could-not-run", check.Outcome)
	}
	if !strings.Contains(check.Because, "log following is off") {
		t.Errorf("reason = %q, want the switched-off reason", check.Because)
	}
}

// TestTheUnheardReasonCountsTheOtherContainers keeps the reason honest
// about scale: one silent container reads differently from a follower
// that has lost the whole cluster, and the operator should be able to
// tell which they have.
func TestTheUnheardReasonCountsTheOtherContainers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		unread []logstream.Unread
		want   string
	}{
		"one": {
			[]logstream.Unread{blind("orders-1", time.Minute, "stream ended")},
			"orders-1 container postgres for 1m0s",
		},
		"two": {
			[]logstream.Unread{
				blind("orders-1", time.Minute, "stream ended"),
				blind("orders-2", time.Minute, "stream ended"),
			},
			"and 1 other container",
		},
		"three": {
			[]logstream.Unread{
				blind("orders-1", time.Minute, "stream ended"),
				blind("orders-2", time.Minute, "stream ended"),
				blind("orders-3", time.Minute, "stream ended"),
			},
			"and 2 other containers",
		},
	} {
		check, _ := evaluateRule(coverageRule, Input{Now: now, HasPods: true, Logs: coveredLogs{unread: tc.unread}})
		if check.Outcome != CheckUnavailable {
			t.Fatalf("%s: outcome = %v, want could-not-run", name, check.Outcome)
		}
		if !strings.Contains(check.Because, tc.want) {
			t.Errorf("%s: reason = %q, want it to carry %q", name, check.Because, tc.want)
		}
	}
}

// TestALogCheckWillNotClearWithoutARoster closes the startup hole. The
// follower follows the containers the pod list names, so without that
// list the console cannot know it is listening to everything that should
// be talking — and a container it has never heard of is one whose
// silence proves nothing.
func TestALogCheckWillNotClearWithoutARoster(t *testing.T) {
	t.Parallel()
	// Nothing unread: every container the follower knows about is being
	// read. It just does not know about all of them.
	check, _ := evaluateRule(coverageRule, Input{Now: now, Logs: coveredLogs{}})
	if check.Outcome != CheckUnavailable {
		t.Fatalf("outcome without a pod roster = %v (%s), want could-not-run", check.Outcome, check.Because)
	}
	if !strings.Contains(check.Because, "which containers should be talking is unknown") {
		t.Errorf("reason = %q, want the roster named as what is missing", check.Because)
	}
}
