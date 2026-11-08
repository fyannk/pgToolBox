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

// Package fault defines the stable, non-sensitive failure vocabulary exposed
// across provider, format, logging, and HTTP boundaries.
package fault

import (
	"context"
	"errors"
)

// Category is deliberately small and never includes an upstream message.
type Category string

const (
	// Unknown is a failure whose meaning cannot be established safely; it is
	// also the category of a nil error, so callers read it as "no known
	// failure" when a result is otherwise successful.
	Unknown Category = "unknown"
	// Canceled means the caller's context was canceled.
	Canceled Category = "canceled"
	// Timeout means a deadline expired.
	Timeout Category = "timeout"
	// InvalidConfig means the configuration was rejected before or by the store.
	InvalidConfig Category = "invalid_configuration"
	// Authentication means the store rejected or could not obtain credentials.
	Authentication Category = "authentication"
	// Authorization means the credentials were valid but lack permission.
	Authorization Category = "authorization"
	// Throttled means the store asked the client to slow down.
	Throttled Category = "throttled"
	// Unavailable means the store or the network could not be reached.
	Unavailable Category = "unavailable"
	// NotFound means the bucket, container or object does not exist.
	NotFound Category = "not_found"
	// Incompatible means repository content does not match the expected format.
	Incompatible Category = "incompatible_format"
	// SafetyLimit means a configured or built-in bound was reached.
	SafetyLimit Category = "safety_limit"
)

// Categorize maps only errors whose meaning can be established safely here.
// Provider adapters add typed mappings without exposing SDK error strings.
func Categorize(err error) Category {
	var categorized interface{ Category() Category }
	switch {
	case err == nil:
		return Unknown
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.As(err, &categorized):
		return categorized.Category()
	default:
		return Unknown
	}
}
