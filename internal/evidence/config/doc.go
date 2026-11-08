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

// Package config is the evidence agent's environment contract: it reads the
// documented variables (Load, LoadOS), applies the defaults, and validates
// the whole configuration before the agent opens a listener, so a bad
// setting stops the start instead of surfacing later.
//
// It covers the runtime mode (RUNTIME_MODE: standalone or agent), the repository and its format, the object-store provider (S3,
// Azure Blob, GCS) and how its credentials are read (Secret files or
// workload identity), the agent's listener and authentication, and the
// scan limits. In team mode (RUNTIME_MODE=agent with AGENT_RESOLVE=cluster)
// the repository is not in the environment: the agent reads it from the
// Cluster and ApplyResolved runs the resolved values through the same
// parsers.
//
// Errors are of type Error and name the variable and a code, never the
// value, so they are safe to log.
package config
