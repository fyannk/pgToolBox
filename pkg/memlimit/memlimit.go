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

// Package memlimit sets the Go runtime's soft memory limit from the
// container's memory limit. Without it the collector paces itself on heap
// growth alone: a process with 400 MiB live can hold 1.5 GiB of garbage and
// meet its cgroup limit (the OOM killer) before it meets a collection.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Fraction of the cgroup limit given to the Go heap; the rest covers
// goroutine stacks, the runtime and memory outside the Go heap.
const Fraction = 0.9

// cgroup files holding the container's memory limit, v2 first.
const (
	cgroupV2 = "/sys/fs/cgroup/memory.max"
	cgroupV1 = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// FromCgroup sets the soft memory limit to Fraction of the cgroup limit, unless
// GOMEMLIMIT is set (the runtime already honours it) or the container has
// no limit. It returns the limit it set, or 0.
func FromCgroup() int64 {
	limit := Limit(os.Getenv, os.ReadFile)
	if limit > 0 {
		debug.SetMemoryLimit(limit)
	}
	return limit
}

// Limit is the soft memory limit FromCgroup would set, or 0.
func Limit(getenv func(string) string, read func(string) ([]byte, error)) int64 {
	if getenv("GOMEMLIMIT") != "" {
		return 0
	}
	for _, path := range []string{cgroupV2, cgroupV1} {
		raw, err := read(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			return 0
		}
		bytes, err := strconv.ParseInt(value, 10, 64)
		// cgroup v1 reports "no limit" as a page-rounded huge number.
		if err != nil || bytes <= 0 || bytes >= 1<<60 {
			return 0
		}
		return int64(float64(bytes) * Fraction)
	}
	return 0
}
