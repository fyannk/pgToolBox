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

// Package scrape parses the instance and pooler exporters' Prometheus
// text exposition into the curated catalog's samples. It is the
// trust-boundary parser: the console's sweep (internal/console/sweep)
// does the reading, and only the catalog's allowlisted names are ever
// folded; everything else in the body is skipped unread.
package scrape

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// target is one catalog entry's claim on a metric name. Several entries
// may claim the same name — cnpg_collector_pg_wal carries the segment
// size, count, floor and ceiling on one name, told apart by its value=
// label — so the parser resolves per target, not per name.
type target struct {
	// key is the catalog key the folded value lands under.
	key string
	// aggregate folds this target's matching label sets.
	aggregate metrics.Aggregate
	// match, when set, restricts the target to label sets whose labels
	// all equal these values.
	match map[string]string
	// instant marks a target from the Instants catalog rather than the
	// time-series Catalog, so the two never collide on a shared key.
	instant bool
}

// targets indexes both catalogs by metric name. needsLabels marks the
// names for which at least one target has a label restriction, which is
// the only case where a line's labels are parsed at all.
func targets(catalog metrics.Catalog) (byName map[string][]target, needsLabels map[string]bool) {
	byName = map[string][]target{}
	needsLabels = map[string]bool{}
	add := func(name string, t target) {
		byName[name] = append(byName[name], t)
		if len(t.match) > 0 {
			needsLabels[name] = true
		}
	}
	for _, def := range catalog.Series {
		for _, name := range def.Names {
			add(name, target{key: def.Key, aggregate: def.Aggregate, match: def.Match})
		}
	}
	for _, def := range catalog.Instants {
		for _, name := range def.Names {
			add(name, target{key: def.Key, aggregate: def.Aggregate, match: def.Match, instant: true})
		}
	}
	return byName, needsLabels
}

// Parse reads Prometheus text format and folds it into both catalogs:
// for each entry the first candidate name that appears wins, and that
// name's matching label sets aggregate per the entry's rule. Anything
// outside the catalogs is skipped unread; malformed lines are skipped
// rather than failing the sweep, so one odd line cannot blind every
// chart, and a NaN — which the exporter uses for "not applicable", as
// on pg_wal{value="volume_size"} without a volume — is never recorded
// as a reading.
func Parse(r io.Reader) (series, instants map[string]float64, err error) {
	return ParseCatalog(metrics.Instance, r)
}

// ParseCatalog is Parse against one named surface.
func ParseCatalog(catalog metrics.Catalog, r io.Reader) (series, instants map[string]float64, err error) {
	type agg struct {
		value float64
		seen  bool
	}
	// folded holds one accumulator per (catalog key, metric name), so
	// the "first candidate name wins" resolution below still has each
	// candidate's own total to choose between.
	folded := map[string]map[string]*agg{}
	byName, needsLabels := targets(catalog)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseLine(line)
		if !ok {
			continue
		}
		wanted := byName[name]
		if len(wanted) == 0 {
			continue
		}
		if !needsLabels[name] {
			labels = ""
		}
		for _, t := range wanted {
			if !labelsMatch(labels, t.match) {
				continue
			}
			perName := folded[t.key]
			if perName == nil {
				perName = map[string]*agg{}
				folded[t.key] = perName
			}
			a := perName[name]
			if a == nil {
				a = &agg{}
				perName[name] = a
			}
			switch {
			case !a.seen:
				a.value, a.seen = value, true
			case t.aggregate == metrics.Max:
				if value > a.value {
					a.value = value
				}
			default:
				a.value += value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, redact.NewError("metrics parse", redact.CategoryInternal, err)
	}

	resolve := func(key string, names []string) (float64, bool) {
		perName := folded[key]
		if perName == nil {
			return 0, false
		}
		for _, name := range names {
			if a := perName[name]; a != nil && a.seen {
				return a.value, true
			}
		}
		return 0, false
	}

	series = map[string]float64{}
	for _, def := range catalog.Series {
		if v, ok := resolve(def.Key, def.Names); ok {
			series[def.Key] = v
		}
	}
	instants = map[string]float64{}
	for _, def := range catalog.Instants {
		if v, ok := resolve(def.Key, def.Names); ok {
			instants[def.Key] = v
		}
	}
	return series, instants, nil
}

// labelsMatch reports whether every wanted label is present in the
// line's label block with the wanted value. An empty match accepts
// every label set, which is the common case.
func labelsMatch(labels string, match map[string]string) bool {
	if len(match) == 0 {
		return true
	}
	for name, want := range match {
		got, ok := lookupLabel(labels, name)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// lookupLabel reads one label out of a Prometheus label block — the
// text between the braces, without them. It walks the pairs rather than
// searching for a substring, so a label named "value" is not matched by
// one named "othervalue".
func lookupLabel(labels, want string) (string, bool) {
	for i := 0; i < len(labels); {
		for i < len(labels) && (labels[i] == ',' || labels[i] == ' ') {
			i++
		}
		start := i
		for i < len(labels) && labels[i] != '=' {
			i++
		}
		if i >= len(labels) {
			return "", false
		}
		name := strings.TrimSpace(labels[start:i])
		i++ // the '='
		if i >= len(labels) || labels[i] != '"' {
			return "", false
		}
		i++ // the opening quote
		var value strings.Builder
		for i < len(labels) && labels[i] != '"' {
			if labels[i] == '\\' && i+1 < len(labels) {
				i++
				switch labels[i] {
				case 'n':
					value.WriteByte('\n')
				default:
					value.WriteByte(labels[i])
				}
			} else {
				value.WriteByte(labels[i])
			}
			i++
		}
		if i >= len(labels) {
			return "", false // unterminated value: the line is malformed
		}
		i++ // the closing quote
		if name == want {
			return value.String(), true
		}
	}
	return "", false
}

// parseLine splits one sample line into its metric name, its label
// block (without the braces, empty when there is none) and its value.
// The optional trailing timestamp is ignored: the sweep time is the
// console's own observation time, which is the claim the store makes.
func parseLine(line string) (name, labels string, value float64, ok bool) {
	rest := line
	if brace := strings.IndexByte(rest, '{'); brace >= 0 {
		name = rest[:brace]
		end := strings.LastIndexByte(rest, '}')
		if end < brace {
			return "", "", 0, false
		}
		labels = rest[brace+1 : end]
		rest = strings.TrimSpace(rest[end+1:])
	} else {
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return "", "", 0, false
		}
		name = fields[0]
		rest = strings.Join(fields[1:], " ")
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", "", 0, false
	}
	return name, labels, v, true
}

// ParseSampleLine splits one exposition sample line into its metric
// name, its label block (without the braces) and its value. ok is
// false for a comment, a malformed line, NaN or an infinity: the
// exporter writes NaN for a NULL column, and a NULL is "not reported",
// never a number.
func ParseSampleLine(line string) (name, labels string, value float64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", 0, false
	}
	return parseLine(line)
}

// ParseLabels reads a whole label block into a map, at most maxLabels
// pairs and each value cut to maxValue bytes. ok is false for a
// malformed block (an unterminated value, a pair without '=', a value
// without quotes or a name that is not a Prometheus label name), which
// the caller drops as a malformed line.
func ParseLabels(labels string, maxLabels, maxValue int) (map[string]string, bool) {
	out := map[string]string{}
	for i := 0; i < len(labels); {
		for i < len(labels) && (labels[i] == ',' || labels[i] == ' ') {
			i++
		}
		if i >= len(labels) {
			break
		}
		start := i
		for i < len(labels) && labels[i] != '=' {
			i++
		}
		if i >= len(labels) {
			return nil, false
		}
		name := strings.TrimSpace(labels[start:i])
		if !validLabelName(name) {
			return nil, false
		}
		i++ // the '='
		if i >= len(labels) || labels[i] != '"' {
			return nil, false
		}
		i++ // the opening quote
		var value strings.Builder
		for i < len(labels) && labels[i] != '"' {
			c := labels[i]
			if c == '\\' && i+1 < len(labels) {
				i++
				c = labels[i]
				if c == 'n' {
					c = '\n'
				}
			}
			if value.Len() < maxValue {
				value.WriteByte(c)
			}
			i++
		}
		if i >= len(labels) {
			return nil, false
		}
		i++ // the closing quote
		if len(out) >= maxLabels {
			return nil, false
		}
		out[name] = value.String()
	}
	return out, true
}

// validLabelName reports whether name is a Prometheus label name,
// [a-zA-Z_][a-zA-Z0-9_]*: what an exporter writes, so anything else is
// a block the parser has misread and must not key a label under.
func validLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
