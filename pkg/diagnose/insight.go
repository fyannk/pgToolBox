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
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

// The insight-pack checks read what the Cluster's own exporters report
// through the pack (pkg/cnpgstatus/insight). They exist only for a
// Cluster that opted in: without the pack there is nothing to read, and
// the check says so with the one-line spec change rather than clearing.
// That absence is a decision, not a fault, so it is a declared
// source-off reason.

// Thresholds of the insight checks. They are console-pinned judgement,
// stated as such on every finding.
const (
	// wraparoundCriticalWithin and wraparoundHighWithin are the time to
	// the stop limit at the observed rate below which a database reads
	// critical and high.
	wraparoundCriticalWithin = 24 * time.Hour
	wraparoundHighWithin     = 7 * 24 * time.Hour
	// wraparoundOverdue is how far past autovacuum_freeze_max_age an age
	// may go before it reads as anti-wraparound vacuum not keeping up.
	// Ages hover just above the setting while autovacuum works through
	// the database; half the setting again does not happen by itself.
	wraparoundOverdue = 1.5
	// idleWarning and idleHigh are the idle-in-transaction ages.
	idleWarning = 10 * time.Minute
	idleHigh    = time.Hour
	// connectionsWarning and connectionsHigh are the shares of the
	// connection slots ordinary roles may take (max_connections minus
	// the reserved ones) in use.
	connectionsWarning = 0.80
	connectionsHigh    = 0.95
)

// insightUnavailable is the reason the insight pack cannot be read,
// empty when it can. It answers for the whole Cluster; the checks then
// ask for the instance and the query they need.
func insightUnavailable(in Input) string {
	switch {
	case !in.HasInsight:
		return reasonMetricsOff
	case len(in.Insight.Instances) == 0:
		return "no instance's metrics have been read yet"
	case !in.Insight.Present():
		for _, r := range in.Insight.Instances {
			if r.Reading.ExpositionCut() {
				return reasonInsightCut
			}
		}
		return reasonInsightOff
	}
	for _, r := range in.Insight.Instances {
		if r.Reading.Present() && !insightUnsupported(r.Reading) {
			return ""
		}
	}
	return reasonInsightUnsupported
}

// reasonInsightCut is a fault, not a decision: the console cut the
// exposition at its size bound, so whether the pack runs is not known.
const reasonInsightCut = "the console cut the exporter's response at its size bound before the insight pack's series, " +
	"so whether the pack runs is not known"

// insightUnsupported reports a reading of PostgreSQL 13 or earlier.
func insightUnsupported(r insight.Reading) bool {
	return r.ServerVersionNum != nil && *r.ServerVersionNum < 140000
}

// insightFresh is how old a reading may be and still describe now:
// three sweeps, never less than two minutes.
func insightFresh(in Input) time.Duration {
	fresh := 2 * time.Minute
	if in.Metrics != nil {
		if d := 3 * in.Metrics.Interval(); d > fresh {
			fresh = d
		}
	}
	return fresh
}

// primaryInsight is the primary's current reading, or the reason there
// is none.
func primaryInsight(in Input) (insight.InstanceReading, string) {
	if reason := insightUnavailable(in); reason != "" {
		return insight.InstanceReading{}, reason
	}
	r, ok := in.Insight.PrimaryReading()
	switch {
	case !ok:
		return r, "no reading of the primary's exporter is held"
	case !r.Reading.Present() && r.Reading.ExpositionCut():
		return r, reasonInsightCut
	case !r.Reading.Present():
		return r, reasonInsightOff
	case insightUnsupported(r.Reading):
		return r, reasonInsightUnsupported
	case in.Now.Sub(r.At) > insightFresh(in):
		return r, fmt.Sprintf("the primary's exporter was last read %s ago, so its current state is unknown",
			roughDuration(in.Now.Sub(r.At)))
	}
	return r, ""
}

// freshInsight is every instance reading the pack that was read
// recently, sorted by instance name, or the reason there is none.
func freshInsight(in Input) ([]insight.InstanceReading, string) {
	if reason := insightUnavailable(in); reason != "" {
		return nil, reason
	}
	var out []insight.InstanceReading
	for _, r := range in.Insight.Instances {
		if r.Reading.Present() && !insightUnsupported(r.Reading) && in.Now.Sub(r.At) <= insightFresh(in) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, "no instance reporting the insight pack has been read recently"
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out, ""
}

func exporterEvidence(r insight.InstanceReading, detail string) Evidence {
	return Evidence{
		Origin: "Instance exporter (insight pack)",
		Object: "Pod/" + r.Instance,
		Detail: detail,
	}
}

// wraparoundDetector reports a database whose transaction id or
// multixact id age is closing on PostgreSQL's limits.
//
// The ages are the primary's own report (age(datfrozenxid),
// mxid_age(datminmxid)). The thresholds are the instance's settings
// when the pack reports them, the Cluster spec's otherwise, PostgreSQL's
// defaults last, and the finding names which. The time left is the
// console's inference from the consumption rate it measured itself.
//
// It implements Detector.
type wraparoundDetector struct{}

func (wraparoundDetector) Name() string { return "insight-wraparound" }

func (wraparoundDetector) Layer() Layer { return LayerPostgreSQL }

func (wraparoundDetector) Describes() string {
	return "a database whose transaction id or multixact age is past its anti-wraparound threshold or near the stop limit (insight pack)"
}

func (wraparoundDetector) Detect(in Input) ([]Finding, string) {
	r, reason := primaryInsight(in)
	if reason != "" {
		return nil, reason
	}
	if len(r.Reading.Databases) == 0 {
		return nil, "the pack's database query reported nothing from the primary " +
			"(PostgreSQL before 14, or the query failed: see the exporter's error count)"
	}
	limits := insight.LimitsFrom(r.Reading.Settings, in.Parameters)
	var xidRate, mxidRate *float64
	if in.Insight.XIDRate != nil {
		xidRate = &in.Insight.XIDRate.PerSecond
	}
	if in.Insight.MXIDRate != nil {
		mxidRate = &in.Insight.MXIDRate.PerSecond
	}
	var findings []Finding
	for _, db := range r.Reading.Databases {
		var worst Severity = -1
		var evidence []Evidence
		var what []string
		judge := func(kind string, age *float64, f func(float64) insight.Forecast, freeze float64) {
			if age == nil {
				return
			}
			fc := f(*age)
			sev, why := wraparoundSeverity(*age, fc, freeze)
			if sev < 0 {
				return
			}
			if sev > worst {
				worst = sev
			}
			what = append(what, kind)
			evidence = append(evidence, exporterEvidence(r, fmt.Sprintf("%s age of database %q: %s (%s); %s",
				kind, db.Name, count(*age), why, horizonText(fc))))
		}
		judge("transaction id", db.XIDAge, func(a float64) insight.Forecast { return insight.ForecastXID(a, limits, 0, xidRate) },
			limits.FreezeMaxAge)
		judge("multixact", db.MXIDAge, func(a float64) insight.Forecast { return insight.ForecastMXID(a, limits, mxidRate) },
			limits.MultixactFreezeMaxAge)
		if worst < 0 {
			continue
		}
		if e, ok := horizonEvidence(r, db.Name); ok {
			evidence = append(evidence, e)
		}
		if assumed := limits.Assumed(); len(assumed) > 0 {
			evidence = append(evidence, Evidence{
				Origin: "Console-pinned",
				Object: "PostgreSQL defaults",
				Detail: "assumed, as neither the instance nor the Cluster spec reports them: " + strings.Join(assumed, ", "),
			})
		}
		findings = append(findings, Finding{
			ID:       "insight-wraparound/" + db.Name,
			Check:    "insight-wraparound",
			Subject:  EntityRef{Kind: "Pod", Name: r.Instance},
			At:       r.At,
			Severity: worst,
			Summary: fmt.Sprintf("Database %q is closing on %s wraparound.",
				db.Name, strings.Join(what, " and ")),
			Detail: "At the stop limit PostgreSQL refuses every statement that needs a new " +
				"transaction id until a VACUUM freezes the oldest rows of this database; " +
				"the application cannot write until then.",
			Evidence: evidence,
			NextSteps: "Find what holds freezing back: a long open transaction, a prepared " +
				"transaction, an abandoned replication slot, or autovacuum disabled on the " +
				"oldest tables (the vacuum screen lists them). Once removed, a VACUUM (FREEZE) " +
				"of the oldest tables, run from pgAdmin, advances the database's age.",
			Link:      "/insight/vacuum",
			LinkLabel: "Wraparound and vacuum",
		})
	}
	return findings, ""
}

// horizonEvidence states what holds the xmin horizon back, as the pack
// reports it: the prepared transactions of the database, replication
// slots and the oldest backends (Cluster-wide).
func horizonEvidence(r insight.InstanceReading, database string) (Evidence, bool) {
	var parts []string
	for _, x := range r.Reading.PreparedXacts {
		if x.Database == database && x.Count != nil && *x.Count > 0 {
			p := count(*x.Count) + " prepared transaction(s)"
			if x.OldestXIDAge != nil {
				p += fmt.Sprintf(", the oldest %s transaction ids old", count(*x.OldestXIDAge))
			}
			parts = append(parts, p)
		}
	}
	if h := r.Reading.Horizons; h != nil {
		for _, c := range []struct {
			what string
			age  *float64
		}{
			{"a replication slot's xmin", h.SlotXminAge},
			{"a logical slot's catalog_xmin", h.SlotCatalogXminAge},
			{"the oldest backend snapshot (backend_xmin)", h.BackendXminAge},
			{"the oldest running transaction", h.BackendXIDAge},
		} {
			if c.age != nil {
				parts = append(parts, fmt.Sprintf("%s is %s transaction ids old", c.what, count(*c.age)))
			}
		}
	}
	if len(parts) == 0 {
		return Evidence{}, false
	}
	return exporterEvidence(r, "what holds freezing back: "+strings.Join(parts, "; ")), true
}

// wraparoundSeverity grades one age, -1 when nothing is wrong, with the
// threshold it crossed in words.
func wraparoundSeverity(age float64, fc insight.Forecast, freeze float64) (Severity, string) {
	soon := func(within time.Duration) bool { return fc.Stop.In != nil && *fc.Stop.In < within }
	switch {
	case age >= insight.WarnLimitAge || fc.Stop.Past || soon(wraparoundCriticalWithin):
		return SeverityCritical, "PostgreSQL is warning on every transaction, or will stop assigning ids within a day"
	case fc.Failsafe.Past || soon(wraparoundHighWithin):
		return SeverityHigh, fmt.Sprintf("past the vacuum failsafe (%s), or within a week of the stop limit",
			count(fc.Failsafe.Threshold))
	case age > wraparoundOverdue*freeze:
		return SeverityWarning, fmt.Sprintf("more than %.1f times the anti-wraparound threshold (%s)",
			wraparoundOverdue, count(freeze))
	}
	return -1, ""
}

func horizonText(fc insight.Forecast) string {
	text := count(fc.Stop.Remaining) + " left to the stop limit"
	switch {
	case fc.Stop.Past:
		return "the stop limit is reached"
	case fc.Stop.In != nil:
		return text + fmt.Sprintf(", about %s at the measured rate", roughDuration(*fc.Stop.In))
	}
	return text + ", no consumption rate measured yet"
}

// idleInTransactionDetector reports sessions left idle inside an open
// transaction, on any instance.
//
// It implements Detector.
type idleInTransactionDetector struct{}

func (idleInTransactionDetector) Name() string { return "insight-idle-in-transaction" }

func (idleInTransactionDetector) Layer() Layer { return LayerPostgreSQL }

func (idleInTransactionDetector) Describes() string {
	return "a session idle inside an open transaction for longer than ten minutes (insight pack)"
}

func (idleInTransactionDetector) Detect(in Input) ([]Finding, string) {
	readings, reason := freshInsight(in)
	if reason != "" {
		return nil, reason
	}
	var findings []Finding
	reported := false
	for _, r := range readings {
		b := r.Reading.Backends
		if b == nil {
			continue
		}
		reported = true
		if b.IdleInTransactionMaxSeconds == nil {
			continue
		}
		age := time.Duration(*b.IdleInTransactionMaxSeconds * float64(time.Second))
		if age <= idleWarning {
			continue
		}
		sev := SeverityWarning
		if age > idleHigh {
			sev = SeverityHigh
		}
		sessions := "at least one session"
		if b.IdleInTransaction != nil {
			sessions = count(*b.IdleInTransaction) + " session(s)"
		}
		findings = append(findings, Finding{
			ID:       "insight-idle-in-transaction/" + r.Instance,
			Check:    "insight-idle-in-transaction",
			Subject:  EntityRef{Kind: "Pod", Name: r.Instance},
			At:       r.At,
			Severity: sev,
			Summary: fmt.Sprintf("A session on %s has been idle in a transaction for %s.",
				r.Instance, roughDuration(age)),
			Detail: "An open transaction keeps its locks and its snapshot: DDL and conflicting " +
				"writes queue behind it, and VACUUM cannot remove any row version newer than " +
				"its snapshot, in every table of the database.",
			Evidence: []Evidence{exporterEvidence(r, fmt.Sprintf(
				"%s idle in transaction; the oldest has been idle for %s",
				sessions, roughDuration(age)))},
			NextSteps: "The connections screen names the database and application holding it. " +
				"Fix the client that does not commit; idle_in_transaction_session_timeout ends " +
				"such sessions on the server side. Ending one now is done from pgAdmin.",
			Link:      "/insight/connections",
			LinkLabel: "Connections",
		})
	}
	if !reported {
		return nil, "the pack's backends query reported nothing (PostgreSQL before 14, or the query failed)"
	}
	return findings, ""
}

// connectionSaturationDetector reports an instance whose client
// connections approach max_connections.
//
// It implements Detector.
type connectionSaturationDetector struct{}

func (connectionSaturationDetector) Name() string { return "insight-connection-saturation" }

func (connectionSaturationDetector) Layer() Layer { return LayerPostgreSQL }

func (connectionSaturationDetector) Describes() string {
	return fmt.Sprintf("an instance with more than %.0f%% of the connection slots ordinary roles may take in use by clients (insight pack)",
		connectionsWarning*100)
}

func (connectionSaturationDetector) Detect(in Input) ([]Finding, string) {
	readings, reason := freshInsight(in)
	if reason != "" {
		return nil, reason
	}
	var findings []Finding
	reported := false
	for _, r := range readings {
		b := r.Reading.Backends
		limit, ok := r.Reading.Settings["max_connections"]
		if b == nil || b.Client == nil || !ok || limit <= 0 {
			continue
		}
		reported = true
		// Reserved slots are not the application's: superuser_reserved_
		// connections, and reserved_connections from PostgreSQL 16.
		reserved := r.Reading.Settings["superuser_reserved_connections"] + r.Reading.Settings["reserved_connections"]
		usable := limit - reserved
		share := 1.0
		if usable > 0 {
			share = *b.Client / usable
		}
		if share <= connectionsWarning {
			continue
		}
		sev := SeverityWarning
		if share > connectionsHigh {
			sev = SeverityHigh
		}
		findings = append(findings, Finding{
			ID:       "insight-connection-saturation/" + r.Instance,
			Check:    "insight-connection-saturation",
			Subject:  EntityRef{Kind: "Pod", Name: r.Instance},
			At:       r.At,
			Severity: sev,
			Summary: fmt.Sprintf("%s has %.0f%% of its usable connection slots in use.",
				r.Instance, math.Floor(share*100)),
			Detail: fmt.Sprintf("Past %s connections (max_connections minus the %s reserved), "+
				"ordinary roles are refused with \"too many clients\".",
				count(limit-reserved), count(reserved)),
			Evidence: []Evidence{exporterEvidence(r, fmt.Sprintf(
				"%s client connections; max_connections %s", count(*b.Client), count(limit)))},
			NextSteps: "The connections screen breaks them down by database, application and state. " +
				"Many idle connections point at oversized client pools, which a Pooler in front " +
				"of the Cluster absorbs; raising max_connections costs memory per connection.",
			Link:      "/insight/connections",
			LinkLabel: "Connections",
		})
	}
	if !reported {
		return nil, "the pack reported no client connection count with max_connections " +
			"(PostgreSQL before 14, or the query failed)"
	}
	return findings, ""
}

// count renders a large integer with thousands separators.
func count(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// roughDuration renders a duration in its largest whole unit pair.
func roughDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return d.Round(time.Second).String()
}
