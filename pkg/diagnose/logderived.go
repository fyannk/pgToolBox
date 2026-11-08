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

package diagnose

import "github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"

// LogObservations is the read side of the continuous matcher. It is an
// interface so diagnose depends on the shape rather than the matcher,
// and so a test needs no stream.
//
// A nil value means log following is off, which every log-backed catalog
// rule reports as "could not run" rather than as nothing found: a
// console that is not reading logs has not ruled out anything in them.
type LogObservations interface {
	Observations() []logstream.Observation
	// Unread is the containers whose streams are not being read at this
	// moment. It is what stops a log check from clearing while the
	// console is blind: a rule looking for a line cannot tell a
	// container that never said it from one nobody was listening to.
	Unread() []logstream.Unread
}
