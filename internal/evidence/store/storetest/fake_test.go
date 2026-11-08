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
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

func TestFakeRejectsCanceledOperationsBeforeDelegation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	fake := &Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		called = true
		return store.Page{}, nil
	}}
	_, err := fake.List(ctx, store.ListRequest{Limit: 1})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("List() = (%v, called %t), want canceled before delegation", err, called)
	}
}

func TestFakeRejectsUnboundedRequests(t *testing.T) {
	t.Parallel()
	fake := &Fake{}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "list without page limit", run: func() error {
			_, err := fake.List(context.Background(), store.ListRequest{})
			return err
		}},
		{name: "list above page ceiling", run: func() error {
			_, err := fake.List(context.Background(), store.ListRequest{Limit: store.MaxPageObjects + 1})
			return err
		}},
		{name: "open without byte ceiling", run: func() error {
			_, err := fake.Open(context.Background(), store.OpenRequest{Key: "metadata"})
			return err
		}},
		{name: "stat without key", run: func() error {
			_, err := fake.Stat(context.Background(), store.StatRequest{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.run(); !errors.Is(err, store.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestSharedReaderContract(t *testing.T) {
	t.Parallel()
	ReaderContract(t, func(*testing.T) store.Reader {
		return &Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			return store.Page{Objects: []store.Object{{Key: "value", Size: 3}}}, nil
		}, StatFunc: func(context.Context, string) (store.Object, error) { return store.Object{Key: "value", Size: 3}, nil }}
	})
}

func TestFakeOpenCutsBodiesAtMaxBytes(t *testing.T) {
	t.Parallel()
	closed := false
	fake := &Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
		return closeRecorder{Reader: strings.NewReader("abcdef"), closed: &closed}, nil
	}}
	body, err := fake.Open(context.Background(), store.OpenRequest{Key: "value", MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "abcd" {
		t.Fatalf("body = %q, %v; want the first MaxBytes bytes", data, err)
	}
	if err := body.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closed %t; want the OpenFunc body closed", err, closed)
	}
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c closeRecorder) Close() error {
	*c.closed = true
	return nil
}
