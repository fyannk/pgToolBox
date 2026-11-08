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
	"testing"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// BenchmarkRunOverTheWholeCatalog measures one complete diagnostic run
// against an input where every source is present and carries something,
// which is the shape a page render would pay for.
func BenchmarkRunOverTheWholeCatalog(b *testing.B) {
	in := everythingObserved()
	rules := Rules()
	b.ReportAllocs()
	for b.Loop() {
		_ = diagnose.Run(in, rules...)
	}
}
