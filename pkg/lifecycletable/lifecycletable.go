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

// Package lifecycletable holds the built-in end-of-life table as the bytes
// every consumer ships: the diagnostics catalog parses it, the operator
// seeds a PgToolBoxConsole's lifecycle ConfigMap with it, and the Helm chart
// carries an identical copy (charts/pgtools/files/lifecycle.json, kept so
// by a test). It imports nothing, so the operator takes the table without
// the console's read model.
//
// PostgreSQL retires each major five years after its release, on the date
// of that quarter's minor release (postgresql.org/support/versioning);
// Kubernetes patches a minor for about fourteen months
// (kubernetes.io/releases). CloudNativePG and the Barman Cloud plugin are
// left empty: their end-of-life dates are announced per release and are not
// pinned here, so their checks say the table lists no lines until a
// deployment adds them.
package lifecycletable

import (
	_ "embed"
	"encoding/json"
)

// Key is the ConfigMap key and file name the console reads.
const Key = "lifecycle.json"

//go:embed lifecycle.json
var table []byte

// JSON returns a copy of the built-in table.
func JSON() []byte { return append([]byte(nil), table...) }

// Reviewed is the day, YYYY-MM-DD, the built-in table was last checked
// against the projects' support pages; "" if the file says none.
func Reviewed() string {
	var head struct {
		Reviewed string `json:"reviewed"`
	}
	if json.Unmarshal(table, &head) != nil {
		return ""
	}
	return head.Reviewed
}
