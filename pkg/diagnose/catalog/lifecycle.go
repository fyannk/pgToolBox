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
	"encoding/json"
	"fmt"
	"time"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/lifecycletable"
)

// Lifecycle is the built-in end-of-life table (pkg/lifecycletable). A
// deployment moves a boundary by editing the console's lifecycle ConfigMap
// rather than waiting for a release.
func Lifecycle() diagnose.Lifecycle {
	l, err := ParseLifecycle(lifecycletable.JSON(), "built-in")
	if err != nil {
		panic(err) // the embedded file is tested; a broken one is a build defect
	}
	return l
}

// ParseLifecycle reads a lifecycle table; source names it under findings.
// Every date must be a YYYY-MM-DD and every line a dotted number, so a
// typo in a ConfigMap is refused whole rather than read half.
func ParseLifecycle(data []byte, source string) (diagnose.Lifecycle, error) {
	var l diagnose.Lifecycle
	if err := json.Unmarshal(data, &l); err != nil {
		return diagnose.Lifecycle{}, fmt.Errorf("lifecycle table: %w", err)
	}
	if l.Reviewed != "" {
		if _, err := time.Parse(time.DateOnly, l.Reviewed); err != nil {
			return diagnose.Lifecycle{}, fmt.Errorf("lifecycle table: reviewed %q is not a YYYY-MM-DD date", l.Reviewed)
		}
	}
	for c, lines := range l.Lines {
		for _, rl := range lines {
			if _, ok := diagnose.ParseLine(rl.Line); !ok {
				return diagnose.Lifecycle{}, fmt.Errorf("lifecycle table: %s line %q is not a dotted version", c, rl.Line)
			}
			if rl.EOL == "" {
				continue
			}
			if _, err := time.Parse(time.DateOnly, rl.EOL); err != nil {
				return diagnose.Lifecycle{}, fmt.Errorf("lifecycle table: %s %s eol %q is not a YYYY-MM-DD date", c, rl.Line, rl.EOL)
			}
		}
	}
	l.Source = source
	return l, nil
}
