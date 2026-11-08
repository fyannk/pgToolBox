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

package lifecycletable

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// The chart ships the built-in table as the lifecycle ConfigMap's default:
// the two files are one table.
func TestChartShipsTheBuiltInTable(t *testing.T) {
	t.Parallel()
	chart, err := os.ReadFile("../../charts/pgtools/files/lifecycle.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chart, JSON()) {
		t.Error("charts/pgtools/files/lifecycle.json differs from pkg/lifecycletable/lifecycle.json: copy the latter over it")
	}
}

func TestReviewedIsADate(t *testing.T) {
	t.Parallel()
	if _, err := time.Parse(time.DateOnly, Reviewed()); err != nil {
		t.Errorf("Reviewed() = %q: %v", Reviewed(), err)
	}
}

func TestJSONIsACopy(t *testing.T) {
	t.Parallel()
	b := JSON()
	b[0] = 'x'
	if JSON()[0] == 'x' {
		t.Error("JSON() hands out the embedded table itself")
	}
}
