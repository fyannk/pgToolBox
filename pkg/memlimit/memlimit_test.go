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

package memlimit

import (
	"errors"
	"testing"
)

func TestLimit(t *testing.T) {
	t.Parallel()
	files := func(content map[string]string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if value, ok := content[path]; ok {
				return []byte(value), nil
			}
			return nil, errors.New("absent")
		}
	}
	noEnv := func(string) string { return "" }
	const gib = 1 << 30
	of := func(limit int64) int64 { return int64(float64(limit) * Fraction) }
	for _, tc := range []struct {
		name   string
		env    string
		files  map[string]string
		expect int64
	}{
		{"cgroup v2 limit", "", map[string]string{cgroupV2: "2147483648\n"}, of(2 * gib)},
		{"cgroup v2 unlimited", "", map[string]string{cgroupV2: "max\n"}, 0},
		{"cgroup v1 limit", "", map[string]string{cgroupV1: "536870912\n"}, of(512 << 20)},
		{"cgroup v1 unlimited", "", map[string]string{cgroupV1: "9223372036854771712\n"}, 0},
		{"v2 wins over v1", "", map[string]string{cgroupV2: "1073741824", cgroupV1: "536870912"}, of(gib)},
		{"no cgroup", "", nil, 0},
		{"garbage", "", map[string]string{cgroupV2: "lots"}, 0},
		{"GOMEMLIMIT set by the operator of the Pod", "1GiB", map[string]string{cgroupV2: "2147483648"}, 0},
	} {
		getenv := noEnv
		if tc.env != "" {
			getenv = func(key string) string {
				if key == "GOMEMLIMIT" {
					return tc.env
				}
				return ""
			}
		}
		if got := Limit(getenv, files(tc.files)); got != tc.expect {
			t.Errorf("%s: Limit = %d, want %d", tc.name, got, tc.expect)
		}
	}
}
