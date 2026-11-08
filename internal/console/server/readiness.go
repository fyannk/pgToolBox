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

package server

import (
	"errors"
	"fmt"
	"sync"
)

// Readiness aggregates named checks. /readyz is 200 only when all pass.
// Its body is constant ("ok" / "not ready"): the reasons go to the log,
// not to whoever can reach the port.
type Readiness struct {
	mu       sync.Mutex
	checks   []check
	draining bool
}

// SetDraining marks the console as shutting down: not ready from now on.
func (r *Readiness) SetDraining() {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
}

type check struct {
	name string
	fn   func() error
}

// Add registers a check.
func (r *Readiness) Add(name string, fn func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, check{name, fn})
}

// Check runs every check and joins the failures.
func (r *Readiness) Check() error {
	r.mu.Lock()
	checks := append([]check(nil), r.checks...)
	draining := r.draining
	r.mu.Unlock()
	if draining {
		return errors.New("shutting down")
	}
	var errs []error
	for _, c := range checks {
		if err := c.fn(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}
