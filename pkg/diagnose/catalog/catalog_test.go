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

package catalog

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// TestCatalogDeclarationsAreComplete is the sanity gate on the data:
// IDs unique across the catalog and the hand-written detectors, every
// rule able to state what it looks for, and every log rule carrying the
// substrings the matcher will be given.
// knownLayer reports whether the layer is in the closed set the screen
// folds checks into. A rule declaring none, or one the screen does not
// know, would vanish from the layer strip.
func knownLayer(layer diagnose.Layer) bool {
	for _, known := range diagnose.Layers() {
		if layer == known {
			return true
		}
	}
	return false
}

func TestCatalogDeclarationsAreComplete(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, detector := range diagnose.Detectors() {
		seen[detector.Name()] = true
		if !knownLayer(detector.Layer()) {
			t.Errorf("detector %q declares layer %q, which is not one the screen knows", detector.Name(), detector.Layer())
		}
	}
	for _, rule := range Rules() {
		if rule.ID == "" || rule.Summary == "" || rule.Component == "" {
			t.Errorf("rule is missing its identity: %+v", rule)
		}
		if !knownLayer(rule.Layer) {
			t.Errorf("rule %q declares layer %q, which is not one the screen knows", rule.ID, rule.Layer)
		}
		if seen[rule.ID] {
			t.Errorf("duplicate check name %q", rule.ID)
		}
		seen[rule.ID] = true
		if rule.Describes == "" && rule.When == nil {
			t.Errorf("rule %q cannot state what it looks for", rule.ID)
		}
		if rule.When == nil && len(rule.Requires) == 0 {
			t.Errorf("rule %q has neither a condition nor a pin, so it would always fire", rule.ID)
		}
		if matcher, ok := diagnose.LogRuleOf(rule.When); ok &&
			len(matcher.Contains) == 0 && len(matcher.Fields) == 0 {
			t.Errorf("log rule %q states nothing to match, so it could never fire", rule.ID)
		}
		if fields, ok := rule.When.(diagnose.LogFields); ok {
			for _, field := range fields.Fields {
				if !field.Valid() {
					t.Errorf("rule %q declares a malformed field test (%+v): a field test names one path "+
						"and sets exactly one of Equals and Contains", rule.ID, field)
				}
			}
		}
		if all, ok := rule.When.(diagnose.AllOf); ok && countLogConditions(all) > 1 {
			t.Errorf("rule %q carries more than one log condition; the matcher keys by rule ID", rule.ID)
		}
	}
	// Every declared cause must be a check that exists, in the catalog or
	// among the detectors: a relation to a check nobody produces would
	// never nest and never be noticed.
	for _, rule := range Rules() {
		for _, relation := range rule.ConsequenceOf {
			if !seen[relation.Cause] {
				t.Errorf("rule %q is declared a consequence of %q, which no check produces", rule.ID, relation.Cause)
			}
			if relation.Cause == rule.ID {
				t.Errorf("rule %q is declared a consequence of itself", rule.ID)
			}
		}
	}
}

// countLogConditions counts the log branches of a composite condition.
func countLogConditions(all diagnose.AllOf) int {
	count := 0
	for _, branch := range all.Of {
		switch condition := branch.(type) {
		case diagnose.LogContains, diagnose.LogFields:
			count++
		case diagnose.AllOf:
			count += countLogConditions(condition)
		}
	}
	return count
}

// TestLogRulesMirrorTheCatalog proves the matcher is fed from the same
// declarations the evaluator reads: every log-backed rule appears, under
// its own ID, with its own substrings.
func TestLogRulesMirrorTheCatalog(t *testing.T) {
	t.Parallel()
	derived := map[string]int{}
	for _, rule := range LogRules() {
		derived[rule.ID] = len(rule.Contains) + len(rule.Fields)
	}
	for _, rule := range Rules() {
		matcher, ok := diagnose.LogRuleOf(rule.When)
		if !ok {
			continue
		}
		tests, present := derived[rule.ID]
		if !present {
			t.Errorf("log rule %q not derived for the matcher", rule.ID)
			continue
		}
		if tests != len(matcher.Contains)+len(matcher.Fields) {
			t.Errorf("rule %q test count diverges", rule.ID)
		}
		delete(derived, rule.ID)
	}
	for id := range derived {
		t.Errorf("matcher rule %q has no catalog declaration", id)
	}
}

// TestDatedKnowledgeIsStillInReview is the console's own discipline
// turned on itself. Pin verification proves the operator's strings still
// say what the catalog claims; nothing proves a support boundary is
// still where it was, because that moves on a calendar and no
// observation changes to announce it. A rule stating one carries the
// date its claim stops being safe to assert unreviewed, and this fails
// once that date passes.
//
// Failing CI is the point. The alternative is a console that keeps
// telling operators a supported version is unsupported, or says nothing
// about one that no longer is — and either is worse than a red test
// with a name that says exactly what to go and read.
//
// The gate fails only where reviewDatesEnv is set (CI's main and
// scheduled jobs): a plain `go test` of a released tree, built after the
// date, logs the overdue review instead of failing, because the people
// building a tarball are not the people who can move the date.
func TestDatedKnowledgeIsStillInReview(t *testing.T) {
	t.Parallel()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	overdue := func(t *testing.T, what, date, whatToDo string) {
		t.Helper()
		days, err := overdueDays(date, today)
		if err != nil {
			t.Errorf("%s carries %q, which is not a YYYY-MM-DD date: %v", what, date, err)
			return
		}
		if days > 0 {
			report := t.Logf
			if enforceReviewDates() {
				report = t.Errorf
			}
			report("%s was to be reviewed by %s, %d days ago: %s", what, date, days, whatToDo)
		}
	}

	for _, rule := range Rules() {
		if rule.ReviewBy == "" {
			continue
		}
		overdue(t, "rule "+rule.ID, rule.ReviewBy,
			"re-read the component's own support policy, move the rule's version constraint "+
				"and its wording to the boundary that holds now, and set the next review date")
	}

	// The newest verified release names itself in the failure, so the
	// reader is told where to start looking. An empty list is its own
	// defect — every CloudNativePG rule is pinned, so verifying against
	// nothing means the whole catalog is unverified — and is reported
	// rather than indexed into.
	if len(cnpg.VerifiedReleases) == 0 {
		t.Fatal("the CloudNativePG verified-release list is empty, so every pinned rule " +
			"is verified against nothing")
	}
	overdue(t, "the CloudNativePG verified-release list", cnpg.VerifiedReviewBy,
		"check what CloudNativePG has released since "+
			cnpg.VerifiedReleases[len(cnpg.VerifiedReleases)-1]+
			", verify the catalog's strings against any new tree, widen the spans it covers, "+
			"and set the next review date")
}

// reviewDatesEnv turns an overdue review date from a logged warning
// into a test failure. CI sets it on main and on the scheduled run.
const reviewDatesEnv = "PGTOOLBOX_ENFORCE_REVIEW_DATES"

// enforceReviewDates reports whether reviewDatesEnv asks for the gate.
func enforceReviewDates() bool { return os.Getenv(reviewDatesEnv) == "1" }

// overdueDays is how many days past its review date a claim is, as of
// the given day. Zero or less is in date: a review due *by* the
// eleventh is not late on the eleventh, so both sides are whole days
// and the comparison is strict.
func overdueDays(date string, today time.Time) (int, error) {
	by, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, err
	}
	return int(today.Sub(by).Hours() / 24), nil
}

// TestOverdueCountsWholeDaysFromTheDayAfter pins the boundary the
// review dates are read against: a claim is in date on its own review
// day and overdue from the next one. Getting this wrong by a day would
// either fail the build early or let a stale boundary ship.
func TestOverdueCountsWholeDaysFromTheDayAfter(t *testing.T) {
	t.Parallel()
	day := func(s string) time.Time {
		parsed, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatalf("bad fixture date %q: %v", s, err)
		}
		return parsed
	}
	for name, tc := range map[string]struct {
		today string
		want  int
	}{
		"a week before":  {"2026-11-05", -7},
		"the day before": {"2026-11-11", -1},
		"the day itself": {"2026-11-12", 0},
		"the day after":  {"2026-11-13", 1},
		"a month after":  {"2026-12-12", 30},
	} {
		got, err := overdueDays("2026-11-12", day(tc.today))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: overdue by %d days, want %d", name, got, tc.want)
		}
	}
	if _, err := overdueDays("12 November 2026", day("2026-11-12")); err == nil {
		t.Error("a date that is not YYYY-MM-DD was accepted")
	}
}

// TestDatedKnowledgeIsDeclaredWhereItExpires proves the field is on the
// rules that need it. A rule whose applicability is a version boundary
// rather than an observation states a fact about a calendar, and one
// that states it without a review date would go stale silently.
func TestDatedKnowledgeIsDeclaredWhereItExpires(t *testing.T) {
	t.Parallel()
	for _, rule := range Rules() {
		if rule.When != nil || rule.ReviewBy != "" {
			continue
		}
		t.Errorf("rule %q fires on its version pins alone, so its claim is a support boundary "+
			"and boundaries move: it needs a ReviewBy date", rule.ID)
	}
}

// TestNoRelationChainLoops refuses a catalog whose causal claims lead
// back to where they started. A cycle means the catalog is asserting
// that each of two findings follows from the other, which is not a
// mechanism anyone could act on.
//
// The screen already defends itself — groupIncidents ignores a relation
// that would loop and leaves the finding as a root — but that defence is
// silent, so a malformed claim would render as flat and nobody would
// learn it was wrong. Catalog defects belong to the build, the way a
// reworded upstream string does.
func TestNoRelationChainLoops(t *testing.T) {
	t.Parallel()
	causes := map[string][]string{}
	ids := make([]string, 0, len(Rules()))
	for _, rule := range Rules() {
		ids = append(ids, rule.ID)
		for _, relation := range rule.ConsequenceOf {
			causes[rule.ID] = append(causes[rule.ID], relation.Cause)
		}
	}

	// A depth-first walk over "is a consequence of". A node still on the
	// current path is a back edge, and the path from it names the loop.
	const (
		unvisited = iota
		onPath
		done
	)
	state := map[string]int{}
	var path []string
	var walk func(id string) []string
	walk = func(id string) []string {
		state[id] = onPath
		path = append(path, id)
		for _, cause := range causes[id] {
			switch state[cause] {
			case onPath:
				from := 0
				for i, seen := range path {
					if seen == cause {
						from = i
						break
					}
				}
				return append(append([]string{}, path[from:]...), cause)
			case unvisited:
				if loop := walk(cause); loop != nil {
					return loop
				}
			}
		}
		state[id] = done
		path = path[:len(path)-1]
		return nil
	}

	for _, id := range ids {
		if state[id] != unvisited {
			continue
		}
		if loop := walk(id); loop != nil {
			t.Fatalf("the catalog's relations loop: %s — each of these is declared a consequence of "+
				"the next, so the chain has no cause to lead back to", strings.Join(loop, " → "))
		}
		path = path[:0]
	}
}

// TestPodScopedRelationsCanActuallyHold refuses a pod-scoped relation
// that could never hold. Relation.Holds admits the pair only when both
// findings name a pod, so a relation pinned to the same pod whose cause
// — or whose own rule — never names one is a claim that does nothing:
// it never nests, and it is reported as a near miss every time, for a
// reason no operator can act on.
//
// A condition PodSubjectOf does not know reads as naming no pod, so a
// new pod-naming condition trips this test the first time a relation
// points at it. That is the intended pressure: classifying it is the
// fix, and the alternative is a relation that quietly stops working.
func TestPodScopedRelationsCanActuallyHold(t *testing.T) {
	t.Parallel()
	when := map[string]diagnose.Condition{}
	for _, rule := range Rules() {
		when[rule.ID] = rule.When
	}
	for _, rule := range Rules() {
		for _, relation := range rule.ConsequenceOf {
			if relation.Scope != diagnose.ScopePod {
				continue
			}
			if diagnose.PodSubjectOf(rule.When) == diagnose.PodSubjectNever {
				t.Errorf("rule %q declares a pod-scoped relation to %q, but its own findings never name "+
					"a pod, so the relation can never hold", rule.ID, relation.Cause)
			}
			cause, known := when[relation.Cause]
			if !known {
				// A cause no rule produces is already reported by
				// TestCatalogDeclarationsAreComplete; a detector's
				// findings are not classified here.
				continue
			}
			if diagnose.PodSubjectOf(cause) == diagnose.PodSubjectNever {
				t.Errorf("rule %q is declared a consequence of %q on the same pod, but %q's findings "+
					"never name a pod, so the relation can never hold", rule.ID, relation.Cause, relation.Cause)
			}
		}
	}
}
