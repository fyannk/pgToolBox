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

package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/preview"
)

// The words are the console's (ui/messages/en.json, preview_*), so the
// change preview screen and this report say the same thing.

var effectWords = map[preview.Effect]string{
	preview.EffectNone:         "No restart",
	preview.EffectReload:       "Reload",
	preview.EffectStart:        "Start",
	preview.EffectRestart:      "Restart",
	preview.EffectRollout:      "Rollout",
	preview.EffectShutdown:     "Shutdown",
	preview.EffectMajorUpgrade: "Major upgrade",
	preview.EffectRefused:      "Refused",
	preview.EffectUnknown:      "Unknown",
}

var effectSummaries = map[preview.Effect]string{
	preview.EffectNone:         "nothing restarts and no configuration is reloaded.",
	preview.EffectReload:       "PostgreSQL reloads its configuration on every instance; no instance restarts.",
	preview.EffectStart:        "PostgreSQL starts again on the instances that were stopped (a fence lifted, or hibernation switched off).",
	preview.EffectRestart:      "PostgreSQL must restart: each replica's Pod is recreated on its volumes, one at a time, and the primary goes last as shown below.",
	preview.EffectRollout:      "every instance's Pod is recreated, one at a time, in the order below.",
	preview.EffectShutdown:     "PostgreSQL stops on the instances below (fencing or hibernation) and stays down until that is lifted.",
	preview.EffectMajorUpgrade: "the Cluster is shut down and upgraded to a new PostgreSQL major version.",
	preview.EffectUnknown:      "the effect could not be determined.",
}

var floorWords = map[preview.Effect]string{
	preview.EffectReload:       "a reload",
	preview.EffectStart:        "a start",
	preview.EffectRestart:      "a restart",
	preview.EffectRollout:      "a rollout",
	preview.EffectShutdown:     "a shutdown",
	preview.EffectMajorUpgrade: "a major upgrade",
}

var contextWords = map[string]string{
	preview.ContextPostmaster: "postmaster (restart)",
	preview.ContextReload:     "reload (sighup)",
	preview.ContextFixed:      "fixed by the operator",
	preview.ContextBlocked:    "refused by the operator",
	preview.ContextAbsent:     "not in this PostgreSQL major (PostgreSQL rejects the configuration)",
}

var reasonWords = map[string]string{
	preview.ReasonImage:             "container image",
	preview.ReasonMajorVersion:      "PostgreSQL major version",
	preview.ReasonPodTemplate:       "Pod template",
	preview.ReasonNewVolume:         "new volume",
	preview.ReasonVolumeResize:      "volume resize",
	preview.ReasonScale:             "number of instances",
	preview.ReasonRestartAnnotation: "restart annotation",
	preview.ReasonHibernation:       "hibernation",
	preview.ReasonFencing:           "fencing",
	preview.ReasonSharedLibraries:   "shared_preload_libraries",
	preview.ReasonManagedExtension:  "extension library added to or removed from shared_preload_libraries by the operator",
	preview.ReasonCertificates:      "TLS certificates",
	preview.ReasonHBA:               "pg_hba rules",
	preview.ReasonSynchronous:       "synchronous replication",
	preview.ReasonReplicaRole:       "replica cluster role",
	preview.ReasonOperatorOnly:      "read by the operator only",
	preview.ReasonUnknownField:      "not in the tables",
}

var actionWords = map[string]string{
	preview.PrimaryNone:           "nothing",
	preview.PrimaryReload:         "configuration reloaded",
	"restart":                     "restarted",
	preview.PrimaryRecreate:       "Pod recreated",
	preview.PrimaryRestartInPlace: "restarted in place",
	preview.PrimarySwitchover:     "switchover to the most advanced replica, then the former primary restarts",
	preview.PrimaryWaitForUser:    "waits for a person (supervised): nothing happens until a switchover or restart is requested",
	preview.PrimaryRestartFirst:   "restarts first, by its own instance manager (a lower hot-standby-sensitive value), then the replicas follow",
	preview.PrimaryMajorUpgrade:   "shut down and upgraded",
	preview.PrimaryShutdown:       "PostgreSQL shut down (fencing or hibernation)",
	preview.PrimaryStart:          "PostgreSQL started",
	preview.PrimaryUnknown:        "cannot be said: a change could not be classified",
}

var roleWords = map[string]string{"primary": "Primary", "replicas": "Replicas", "all": "All instances"}

// word returns the wording of a code, or the code itself when the tables
// have none (a newer preview package), so nothing is silently dropped.
func word[K ~string](m map[K]string, k K) string {
	if w, ok := m[k]; ok {
		return w
	}
	return string(k)
}

func writeText(w io.Writer, rep report) {
	var b strings.Builder
	for i, c := range rep.Clusters {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Cluster %s: %s\n", clean(key{c.Namespace, c.Name}.String()), c.Change)
		switch c.Change {
		case changeRemoved:
			b.WriteString("  Removed — deleting the Cluster deletes its instances.\n")
		case changeUnchanged:
			b.WriteString("  Unchanged — the spec and annotations the preview reads are the same.\n")
		case changeAdded:
			b.WriteString("  Added — a new Cluster: no running instance is touched.\n")
		default:
			writePreview(&b, cleanPreview(*c.Preview), rep.CNPGVersion)
			if f := c.FewerInstances; f != nil {
				fmt.Fprintf(&b, "  Fewer instances — %d → %d: replicas are removed and fewer remain to fail over to.\n", f.Before, f.After)
			}
		}
	}
	if rep.Ignored > 0 {
		fmt.Fprintf(&b, "\n%d document(s) that are not CloudNativePG Clusters were ignored.\n", rep.Ignored)
	}
	if rep.Disruptive {
		b.WriteString("\nDisruptive: at least one Cluster restarts, rolls out, shuts down, is upgraded, loses instances or is removed, carries a refused change, or has an unknown effect.\n")
	} else {
		b.WriteString("\nNot disruptive: at most a configuration reload or a start.\n")
	}
	_, _ = io.WriteString(w, b.String())
}

func writePreview(b *strings.Builder, p preview.Preview, cnpgVersion string) {
	unknown := p.Effect == preview.EffectUnknown || p.Unknown > 0
	if !p.Verified && !unknown {
		major := "not reported"
		if p.PostgresMajor > 0 {
			major = strconv.Itoa(p.PostgresMajor)
		}
		if cnpgVersion == "" {
			fmt.Fprintf(b, "  Unverified: no CloudNativePG version was given (--cnpg-version), so the operator's rules cannot be matched to it (PostgreSQL %s). Check the CloudNativePG documentation before relying on it.\n", major)
		} else {
			fmt.Fprintf(b, "  Unverified: PostgreSQL %s or CloudNativePG %s is not covered by the tables this preview reads. Check the CloudNativePG documentation before relying on it.\n", major, cnpgVersion)
		}
	}
	if unknown {
		floor, ok := floorWords[p.AtLeast]
		lead := "Unknown"
		if ok {
			lead = "Unknown, at least " + floor
		}
		fmt.Fprintf(b, "  %s — %d change(s) could not be classified: their effect is unknown. ", lead, p.Unknown)
		if ok {
			b.WriteString("What is known already does this; the unclassified change(s) may do more.\n")
		} else {
			b.WriteString("None of the classified changes restarts anything, but a restart cannot be ruled out.\n")
		}
	} else {
		fmt.Fprintf(b, "  %s — %s\n", word(effectWords, p.Effect), word(effectSummaries, p.Effect))
	}
	if n := refused(p); n > 0 {
		fmt.Fprintf(b, "  Refused — the operator refuses %d change(s) below: the spec cannot carry this change.\n", n)
	}
	strategy := "the operator updates the primary on its own"
	if p.Strategy == "supervised" {
		strategy = "the operator waits for a person before touching the primary"
	}
	fmt.Fprintf(b, "  Primary update strategy: %s (%s)\n", p.Strategy, strategy)
	fmt.Fprintf(b, "  Primary update method: %s\n", p.Method)
	fmt.Fprintf(b, "  What happens to the primary: %s\n", word(actionWords, p.Primary))
	if p.PostgresMajor > 0 {
		fmt.Fprintf(b, "  PostgreSQL major: %d\n", p.PostgresMajor)
	}
	if len(p.Steps) > 0 {
		if unknown {
			b.WriteString("  Order of the update (at least):\n")
		} else {
			b.WriteString("  Order of the update:\n")
		}
		byRole := false
		for i, s := range p.Steps {
			names := ""
			if len(s.Instances) > 0 {
				names = " (" + strings.Join(s.Instances, ", ") + ")"
			} else {
				byRole = true
			}
			fmt.Fprintf(b, "    %d. %s%s: %s\n", i+1, word(roleWords, s.Role), names, word(actionWords, s.Action))
		}
		if byRole {
			b.WriteString("    A manifest holds the spec only: instance names are the Cluster's at the time of the update, so the order is given by role.\n")
		}
	}
	if len(p.Parameters) > 0 {
		b.WriteString("  PostgreSQL parameters:\n")
		for _, c := range p.Parameters {
			context := "—"
			if c.Context != "" {
				context = word(contextWords, c.Context)
			}
			fmt.Fprintf(b, "    %s: %s → %s — %s, %s\n", c.Name, value(c.Before), value(c.After), word(effectWords, c.Effect), context)
			if c.Decrease {
				b.WriteString("      Lower value of a hot-standby-sensitive setting: the primary restarts first (under supervised, once a person acts), then the replicas.\n")
			}
		}
	}
	if len(p.Fields) > 0 {
		b.WriteString("  Other fields:\n")
		for _, f := range p.Fields {
			fmt.Fprintf(b, "    %s: %s — %s\n", f.Path, word(effectWords, f.Effect), word(reasonWords, f.Reason))
		}
	}
}

func value(v string) string {
	if v == "" {
		return "unset"
	}
	return v
}

// clean escapes the control characters of a manifest-sourced string (C0,
// C1 and so ESC, and the Unicode line and paragraph separators), so a
// manifest cannot start a line of its own in a CI log (a workflow command
// such as "::stop-commands::") or move the terminal cursor.
func clean(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafeRune(r) {
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

// cleanPreview is p with every manifest-sourced string cleaned.
func cleanPreview(p preview.Preview) preview.Preview {
	p.Strategy, p.Method = clean(p.Strategy), clean(p.Method)
	params := make([]preview.ParameterChange, len(p.Parameters))
	for i, c := range p.Parameters {
		c.Name, c.Before, c.After = clean(c.Name), clean(c.Before), clean(c.After)
		params[i] = c
	}
	fields := make([]preview.FieldChange, len(p.Fields))
	for i, f := range p.Fields {
		f.Path = clean(f.Path)
		fields[i] = f
	}
	steps := make([]preview.Step, len(p.Steps))
	for i, s := range p.Steps {
		names := make([]string, len(s.Instances))
		for j, n := range s.Instances {
			names[j] = clean(n)
		}
		s.Instances = names
		steps[i] = s
	}
	p.Parameters, p.Fields, p.Steps = params, fields, steps
	return p
}
