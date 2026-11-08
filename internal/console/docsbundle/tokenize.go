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

package docsbundle

import (
	"strings"
	"unicode"
)

// joiners glue the parts of an identifier together without ending the word:
// history.persistent, cluster-ref, pgtoolbox.fyannk.dev/v1alpha1, snake_case.
func isJoiner(r rune) bool {
	return r == '.' || r == '-' || r == '_' || r == '/'
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "of": {}, "to": {}, "in": {}, "and": {}, "or": {},
	"is": {}, "are": {}, "for": {}, "on": {}, "it": {}, "be": {}, "by": {}, "as": {},
	"at": {}, "with": {}, "that": {}, "this": {}, "from": {},
}

// tokenize turns text into index terms. A run of letters and digits glued by
// '.', '-', '_' or '/' is one compound: it yields the whole compound
// (history.persistent), each of its parts (history, persistent) and, for a
// part in camel case, its words (InstanceCABundleComplete yields the whole
// lower-cased identifier and instance, ca, bundle, complete). Every term is
// lower-cased and lightly de-pluralised, queries and documents alike, so
// "conditions" finds "condition". The order of the result follows the text;
// terms repeat once per occurrence.
func tokenize(s string) []string {
	scanned := scan(s)
	out := make([]string, 0, len(scanned))
	for _, t := range scanned {
		out = append(out, t.term)
	}
	return out
}

// term is an index term; whole marks the one that stands for a compound or a
// CamelCase identifier as a whole, which a query weighs more than its parts.
type term struct {
	term  string
	whole bool
}

func scan(s string) []term {
	var out []term
	runes := []rune(s)
	for i := 0; i < len(runes); {
		if !isWordRune(runes[i]) {
			i++
			continue
		}
		// Extend over word runes and joiners that sit between word runes.
		j := i
		for j < len(runes) {
			if isWordRune(runes[j]) {
				j++
				continue
			}
			if isJoiner(runes[j]) && j+1 < len(runes) && isWordRune(runes[j+1]) {
				j++
				continue
			}
			break
		}
		out = appendCompound(out, string(runes[i:j]))
		i = j
	}
	return out
}

func appendCompound(out []term, compound string) []term {
	parts := splitJoiners(compound)
	if len(parts) > 1 {
		out = appendTerm(out, strings.ToLower(compound), true)
	}
	for _, part := range parts {
		words := splitCamel(part)
		if len(words) > 1 {
			out = appendTerm(out, strings.ToLower(part), true)
		}
		for _, w := range words {
			out = appendTerm(out, strings.ToLower(w), false)
		}
	}
	return out
}

func appendTerm(out []term, t string, whole bool) []term {
	if t == "" {
		return out
	}
	if _, stop := stopwords[t]; stop {
		return out
	}
	return append(out, term{stem(t), whole})
}

func splitJoiners(s string) []string {
	return strings.FieldsFunc(s, isJoiner)
}

// splitCamel cuts a word at lower-to-upper transitions and before the last
// capital of an acronym: InstanceCABundleComplete -> Instance, CA, Bundle,
// Complete. Digits stay with the letters before them (v1alpha1, sha256).
func splitCamel(s string) []string {
	runes := []rune(s)
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			boundary = true
		}
		if boundary {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	return append(words, string(runes[start:]))
}

// stem removes a plural ending: conditions -> condition, policies -> policy,
// classes -> class. Words that merely end in s (status, access, is) stay.
func stem(t string) string {
	n := len(t)
	switch {
	case n > 4 && strings.HasSuffix(t, "ies"):
		return t[:n-3] + "y"
	case n > 5 && strings.HasSuffix(t, "sses"):
		return t[:n-2]
	case n > 3 && strings.HasSuffix(t, "s") &&
		!strings.HasSuffix(t, "ss") && !strings.HasSuffix(t, "us") && !strings.HasSuffix(t, "is"):
		return t[:n-1]
	}
	return t
}
