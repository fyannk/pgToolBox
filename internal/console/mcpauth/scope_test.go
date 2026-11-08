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

package mcpauth

import (
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

func TestHasScope(t *testing.T) {
	cases := []struct {
		name  string
		p     *auth.Principal
		scope string
		want  bool
	}{
		{"no principal", nil, ScopeRead, false},
		{"raw token reads", &auth.Principal{Provider: auth.BearerProvider}, ScopeRead, true},
		{"raw token operates (RBAC bounds it)", &auth.Principal{Provider: auth.BearerProvider}, ScopeOperate, true},
		{"read grant reads", &auth.Principal{Provider: auth.GrantProvider, Scopes: []string{ScopeRead}}, ScopeRead, true},
		{"read grant cannot operate", &auth.Principal{Provider: auth.GrantProvider, Scopes: []string{ScopeRead}}, ScopeOperate, false},
		{"operate grant operates", &auth.Principal{Provider: auth.GrantProvider, Scopes: []string{ScopeRead, ScopeOperate}}, ScopeOperate, true},
		{"grant without scopes", &auth.Principal{Provider: auth.GrantProvider}, ScopeRead, false},
	}
	for _, c := range cases {
		if got := HasScope(c.p, c.scope); got != c.want {
			t.Errorf("%s: HasScope = %v, want %v", c.name, got, c.want)
		}
	}
}
