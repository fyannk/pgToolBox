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

// Package redact provides last-resort boundary redaction. Boundary code should
// still prefer stable error categories over rendering raw upstream errors.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const replacement = "[REDACTED]"

// MaxKnownValues bounds the secret values one redactor remembers. Rotated
// values are added; past the bound the oldest added values are forgotten
// first, the generic credential patterns still apply to them.
const MaxKnownValues = 64

var (
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]+`)
	querySecretPattern   = regexp.MustCompile(`(?i)([?&](?:access_token|credential|password|secret|sig|signature|token)=)[^&\s#]+`)
	assignmentPattern    = regexp.MustCompile(`(?i)((?:access[_-]?key|account[_-]?key|client[_-]?secret|password|sas[_-]?token|secret[_-]?key|session[_-]?token)\s*[=:]\s*)[^\s,;]+`)
)

// Redactor replaces configured canaries before generic credential-shaped
// text. It is safe for concurrent use; Add extends it after a rotation.
type Redactor struct {
	mu     sync.Mutex
	values []string
	known  atomic.Pointer[strings.Replacer]
}

// New builds a redactor. Longer values are replaced first.
func New(values ...[]byte) *Redactor {
	redactor := &Redactor{}
	redactor.Add(values...)
	return redactor
}

// Add remembers more secret values, such as credentials re-read after a
// rotation. Empty and already known values are ignored.
func (r *Redactor) Add(values ...[]byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		text := string(value)
		if text == "" || containsString(r.values, text) {
			continue
		}
		r.values = append(r.values, text)
	}
	if len(r.values) > MaxKnownValues {
		r.values = append([]string(nil), r.values[len(r.values)-MaxKnownValues:]...)
	}
	known := append([]string(nil), r.values...)
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	pairs := make([]string, 0, len(known)*2)
	for _, value := range known {
		pairs = append(pairs, value, replacement)
	}
	r.known.Store(strings.NewReplacer(pairs...))
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// String redacts a bounded diagnostic string.
func (r *Redactor) String(value string) string {
	if r != nil {
		if known := r.known.Load(); known != nil {
			value = known.Replace(value)
		}
	}
	value = authorizationPattern.ReplaceAllString(value, `${1}`+replacement)
	value = querySecretPattern.ReplaceAllString(value, `${1}`+replacement)
	return assignmentPattern.ReplaceAllString(value, `${1}`+replacement)
}
