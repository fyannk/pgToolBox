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

package observe

import (
	"context"
	"sync"
	"time"
)

// fakeClock advances one second per Now call and records waits without
// sleeping, so collector tests run instantly and deterministically.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []time.Duration
	nows  int
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nows++
	f.t = f.t.Add(time.Second)
	return f.t
}

func (f *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (f *fakeClock) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}
