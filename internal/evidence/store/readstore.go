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

// Package store is the complete object-store capability visible to domain
// code. Mutation operations are intentionally impossible through Reader.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// Bounds every Reader request must respect.
const (
	// MaxPageObjects is the largest page a List may request.
	MaxPageObjects = 1_000
	// MaxKeyBytes bounds an object key or list prefix.
	MaxKeyBytes = 16 * 1024
	// MaxCursorBytes bounds an opaque list cursor.
	MaxCursorBytes = 16 * 1024
)

// ErrInvalidRequest is returned for a request that fails its Validate or
// that a store rejects before calling the provider.
var ErrInvalidRequest = errors.New("invalid bounded read-store request")

// Object contains only allowlisted provider-neutral metadata.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// ListRequest is prefix-scoped and page-bounded.
type ListRequest struct {
	Prefix string
	Cursor string
	Limit  int
}

// Validate checks the page limit and the prefix and cursor sizes.
func (r ListRequest) Validate() error {
	if r.Limit < 1 || r.Limit > MaxPageObjects || len(r.Prefix) > MaxKeyBytes || len(r.Cursor) > MaxCursorBytes {
		return ErrInvalidRequest
	}
	return nil
}

// Page contains one bounded page. Cursor is opaque to every layer except the
// matching provider adapter.
type Page struct {
	Objects    []Object
	NextCursor string
}

// OpenRequest requires a positive byte ceiling; adapters must never return
// more content than MaxBytes.
type OpenRequest struct {
	Key      string
	MaxBytes int64
}

// Validate requires a bounded, non-empty key and a positive MaxBytes.
func (r OpenRequest) Validate() error {
	if r.Key == "" || len(r.Key) > MaxKeyBytes || r.MaxBytes < 1 {
		return ErrInvalidRequest
	}
	return nil
}

// StatRequest asks for one object's metadata.
type StatRequest struct {
	Key string
}

// Validate requires a bounded, non-empty key.
func (r StatRequest) Validate() error {
	if r.Key == "" || len(r.Key) > MaxKeyBytes {
		return ErrInvalidRequest
	}
	return nil
}

// Reader is the structural read-only boundary for all repository formats.
type Reader interface {
	List(context.Context, ListRequest) (Page, error)
	Open(context.Context, OpenRequest) (io.ReadCloser, error)
	Stat(context.Context, StatRequest) (Object, error)
}
