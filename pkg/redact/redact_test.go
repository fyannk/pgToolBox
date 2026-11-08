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

package redact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// canary values that must never survive into safe output.
const (
	canaryToken = "Bearer sekret-canary-token"
	canaryURL   = "https://user:sekret-canary-pass@10.0.0.1:6443/api/v1/namespaces/payments/secrets"
)

// hostileCause simulates a raw client error embedding a request URL and
// an authorization header.
func hostileCause() error {
	return fmt.Errorf("GET %s: unauthorized: %s", canaryURL, canaryToken)
}

func TestRedactorSafeNeverLeaksCauseText(t *testing.T) {
	t.Parallel()
	err := NewError("cluster get", CategoryForbidden, hostileCause())
	for name, out := range map[string]string{
		"Safe":  Safe(err),
		"Error": err.Error(),
	} {
		if strings.Contains(out, "sekret-canary") {
			t.Errorf("%s leaks canary: %q", name, out)
		}
		if strings.Contains(out, "10.0.0.1") {
			t.Errorf("%s leaks request URL: %q", name, out)
		}
	}
	if err.Error() != "cluster get: forbidden" {
		t.Errorf("Error() = %q, want op and category only", err.Error())
	}
}

func TestRedactorSafeOnWrappedHostileError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("refreshing: %w", hostileCause())
	if got := Safe(wrapped); got != string(CategoryInternal) {
		t.Errorf("Safe = %q, want %q", got, CategoryInternal)
	}
	if strings.Contains(Safe(wrapped), "sekret-canary") {
		t.Error("Safe leaks canary from an uncategorized error")
	}
}

func TestRedactorCategorize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want Category
	}{
		{"nil-safe empty", nil, CategoryInternal},
		{"canceled", context.Canceled, CategoryCanceled},
		{"wrapped canceled", fmt.Errorf("watch: %w", context.Canceled), CategoryCanceled},
		{"deadline", context.DeadlineExceeded, CategoryTimeout},
		{"wrapped deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), CategoryTimeout},
		{"categorized", NewError("probe", CategoryUnavailable, nil), CategoryUnavailable},
		{"categorized wrapped", fmt.Errorf("readyz: %w", NewError("probe", CategoryNotFound, nil)), CategoryNotFound},
		{"plain", errors.New("boom"), CategoryInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "nil-safe empty" {
				if got := Safe(nil); got != "" {
					t.Errorf("Safe(nil) = %q, want empty", got)
				}
				return
			}
			if got := Categorize(tc.err); got != tc.want {
				t.Errorf("Categorize = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactorUnwrapKeepsCauseInProcess(t *testing.T) {
	t.Parallel()
	cause := context.DeadlineExceeded
	err := NewError("cluster get", CategoryTimeout, cause)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("cause is not reachable through errors.Is")
	}
}
