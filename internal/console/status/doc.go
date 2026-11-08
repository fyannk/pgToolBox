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

// Package status is the console's read model service: it assembles the
// view models of internal/console/viewmodel from the informer cache, the
// /pg/status sweep and the diagnostics runner, applying the per-minor
// tables of pkg/cnpgstatus. Every method is an in-memory read of
// published snapshots — no API call, no network — so the API layer
// (internal/console/api) can serve it on any request path. A source
// whose list failed is reported as not read, never as empty.
// Authorization is not here: the API removes what the caller may not get
// before serving.
package status
