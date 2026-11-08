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

package operations

import (
	"net/http"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Rejected is a request refused with a status and a display-safe text:
// nothing was written. The API maps it onto a problem.
type Rejected struct {
	Status int
	Text   string
}

// Error implements error.
func (r *Rejected) Error() string { return r.Text }

// HTTPStatus is the problem status.
func (r *Rejected) HTTPStatus() int { return r.Status }

// SafeText is the display-safe text.
func (r *Rejected) SafeText() string { return r.Text }

// Rejection builds a Rejected.
func Rejection(status int, text string) error { return &Rejected{Status: status, Text: text} }

// CheckConfirmation enforces the typed confirmation server-side: the
// Cluster name, or the target instance for promote. The UI asks for it,
// but a bug or a script must not skip it.
func CheckConfirmation(cluster string, req viewmodel.OperationRequest) error {
	switch req.Kind {
	case viewmodel.OperationBackup, viewmodel.OperationReload, viewmodel.OperationRestart, viewmodel.OperationRestoreDrill:
		if req.Target != "" {
			return Rejection(http.StatusUnprocessableEntity, string(req.Kind)+" takes no target")
		}
		if req.Confirmation != cluster {
			return Rejection(http.StatusUnprocessableEntity, "type the Cluster name, "+cluster+", to confirm")
		}
	case viewmodel.OperationPromote:
		if req.Target == "" {
			return Rejection(http.StatusUnprocessableEntity, "promote needs a target instance")
		}
		if req.Confirmation != req.Target {
			return Rejection(http.StatusUnprocessableEntity, "type the target instance name, "+req.Target+", to confirm")
		}
	default:
		return Rejection(http.StatusUnprocessableEntity, "no such operation")
	}
	return nil
}
