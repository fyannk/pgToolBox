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

// Package redact defines the stable error categories and the safe error
// representations that may cross an output boundary. Raw errors from
// clients can embed request URLs, tokens, and header values; nothing in
// a log line, metric, or HTTP response may carry an error's raw text.
// Only the category and the operation label are safe.
package redact

import (
	"context"
	"errors"
)

// Category is a stable, output-safe classification of a failure. The set
// is closed: rendering, logging, and metrics key on these values, so a
// new category is an interface change, not a convenience.
type Category string

// The closed set of categories.
const (
	// CategoryCanceled reports that the caller canceled the operation.
	CategoryCanceled Category = "canceled"
	// CategoryTimeout reports that the operation exceeded its deadline.
	CategoryTimeout Category = "timeout"
	// CategoryForbidden reports a permission denial. It renders as "not
	// granted" and is never retried in a loop.
	CategoryForbidden Category = "forbidden"
	// CategoryNotFound reports that the requested object does not exist.
	CategoryNotFound Category = "notfound"
	// CategoryUnavailable reports that a required capability is not
	// currently usable.
	CategoryUnavailable Category = "unavailable"
	// CategoryInternal reports every failure that matches no other
	// category.
	CategoryInternal Category = "internal"
)

// Categorizer is implemented by errors that know their own category.
type Categorizer interface {
	// Category reports the output-safe classification of the error.
	Category() Category
}

// Error is a categorized error that is safe to render and log: its
// message is the operation label and the category, never wrapped detail.
// The underlying cause remains reachable through Unwrap for in-process
// branching, but callers must emit only Safe representations.
type Error struct {
	// Op is a stable, human-chosen operation label, such as a route name
	// or a client call site. It must never be built from request data.
	Op string
	// Cat is the output-safe classification.
	Cat Category
	// Cause is the underlying error, if any. It is never rendered.
	Cause error
}

// NewError builds a categorized error for the given operation.
func NewError(op string, cat Category, cause error) *Error {
	return &Error{Op: op, Cat: cat, Cause: cause}
}

// Error returns "op: category" and nothing else.
func (e *Error) Error() string {
	return e.Op + ": " + string(e.Cat)
}

// Category reports the output-safe classification of the error.
func (e *Error) Category() Category {
	return e.Cat
}

// Unwrap exposes the cause for errors.Is and errors.As. The cause is for
// in-process inspection only and must never reach an output boundary.
func (e *Error) Unwrap() error {
	return e.Cause
}

// Categorize maps any error to its output-safe category. Errors carrying
// their own category win; context cancellation and deadline expiry map to
// their categories; everything else is internal.
func Categorize(err error) Category {
	var c Categorizer
	if errors.As(err, &c) {
		return c.Category()
	}
	if errors.Is(err, context.Canceled) {
		return CategoryCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CategoryTimeout
	}
	return CategoryInternal
}

// Safe returns the only string form of an error that may cross an output
// boundary: the category name. A nil error yields the empty string.
func Safe(err error) string {
	if err == nil {
		return ""
	}
	return string(Categorize(err))
}
