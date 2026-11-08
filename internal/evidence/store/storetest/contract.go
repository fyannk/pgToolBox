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

package storetest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// ReaderContract verifies the provider-neutral safety surface. Provider
// packages call it with a deterministic adapter fixture in their unit and
// integration suites.
func ReaderContract(t *testing.T, newReader func(t *testing.T) store.Reader) {
	t.Helper()
	t.Run("invalid requests are rejected before I/O", func(t *testing.T) {
		reader := newReader(t)
		if _, err := reader.List(context.Background(), store.ListRequest{}); !errors.Is(err, store.ErrInvalidRequest) {
			t.Fatalf("List() = %v", err)
		}
		if _, err := reader.Open(context.Background(), store.OpenRequest{}); !errors.Is(err, store.ErrInvalidRequest) {
			t.Fatalf("Open() = %v", err)
		}
		if _, err := reader.Stat(context.Background(), store.StatRequest{}); !errors.Is(err, store.ErrInvalidRequest) {
			t.Fatalf("Stat() = %v", err)
		}
	})
	t.Run("open is bounded", func(t *testing.T) {
		reader := newReader(t)
		body, err := reader.Open(context.Background(), store.OpenRequest{Key: "value", MaxBytes: 2})
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 2 {
			t.Fatalf("unbounded open returned %d bytes", len(data))
		}
	})
	t.Run("list and stat preserve only allowlisted facts", func(t *testing.T) {
		reader := newReader(t)
		page, err := reader.List(context.Background(), store.ListRequest{Limit: 1})
		if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != "value" || page.Objects[0].Size != 3 {
			t.Fatalf("List() = %#v, %v", page, err)
		}
		object, err := reader.Stat(context.Background(), store.StatRequest{Key: "value"})
		if err != nil || object.Key != "value" || object.Size != 3 || (!object.LastModified.IsZero() && object.LastModified.Location() != time.UTC) {
			t.Fatalf("Stat() = %#v, %v", object, err)
		}
	})
	t.Run("caller cancellation reaches provider operations", func(t *testing.T) {
		reader := newReader(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := reader.List(ctx, store.ListRequest{Limit: 1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("List() error = %v, want context canceled", err)
		}
	})
}
