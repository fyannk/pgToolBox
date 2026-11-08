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

package application

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// newScopeConfinedReader confines reader to the producing mode's scope
// prefixes: one for Barman (the prefixConfinedReader, unchanged), two for
// pgBackRest (backup/<stanza>/ and archive/<archive stanza>/).
func newScopeConfinedReader(reader store.Reader, prefixes []string) (store.Reader, error) {
	if len(prefixes) == 1 {
		return newPrefixConfinedReader(reader, prefixes[0])
	}
	return newMultiPrefixReader(reader, prefixes)
}

// multiPrefixReader reads only under a few disjoint prefixes. A listing
// without a prefix walks them in key order, one after the other, behind a
// composite cursor "<prefix index>|<provider cursor>"; a listing with a
// prefix must sit inside one of them and passes the cursor through.
type multiPrefixReader struct {
	reader   store.Reader
	prefixes []string
}

// maxConfinedPrefixes keeps the prefix index of the cursor one digit.
const maxConfinedPrefixes = 9

func newMultiPrefixReader(reader store.Reader, prefixes []string) (*multiPrefixReader, error) {
	if reader == nil || len(prefixes) < 2 || len(prefixes) > maxConfinedPrefixes {
		return nil, errors.New("invalid store confinement")
	}
	sorted := slices.Clone(prefixes)
	slices.Sort(sorted)
	for index, prefix := range sorted {
		if prefix == "" || !strings.HasSuffix(prefix, "/") || len(prefix) > store.MaxKeyBytes {
			return nil, errors.New("invalid store confinement")
		}
		// Disjoint prefixes: no prefix contains another, so every key is
		// listed once and the walk stays in key order.
		if index > 0 && strings.HasPrefix(prefix, sorted[index-1]) {
			return nil, errors.New("invalid store confinement: overlapping prefixes")
		}
	}
	return &multiPrefixReader{reader: reader, prefixes: sorted}, nil
}

func (r *multiPrefixReader) within(key string) bool {
	return slices.ContainsFunc(r.prefixes, func(prefix string) bool { return strings.HasPrefix(key, prefix) })
}

func (r *multiPrefixReader) List(ctx context.Context, request store.ListRequest) (store.Page, error) {
	if request.Prefix != "" {
		if !r.within(request.Prefix) {
			return store.Page{}, errStoreConfinement
		}
		return r.checked(r.reader.List(ctx, request))
	}
	index, inner := 0, ""
	if request.Cursor != "" {
		head, rest, ok := strings.Cut(request.Cursor, "|")
		parsed, err := strconv.Atoi(head)
		if !ok || err != nil || len(head) != 1 || parsed < 0 || parsed >= len(r.prefixes) {
			return store.Page{}, store.ErrInvalidRequest
		}
		index, inner = parsed, rest
	}
	page, err := r.checked(r.reader.List(ctx, store.ListRequest{Prefix: r.prefixes[index], Cursor: inner, Limit: request.Limit}))
	if err != nil {
		return store.Page{}, err
	}
	switch {
	case page.NextCursor != "":
		page.NextCursor = strconv.Itoa(index) + "|" + page.NextCursor
	case index+1 < len(r.prefixes):
		page.NextCursor = strconv.Itoa(index+1) + "|"
	}
	if len(page.NextCursor) > store.MaxCursorBytes {
		return store.Page{}, errStoreConfinement
	}
	return page, nil
}

func (r *multiPrefixReader) checked(page store.Page, err error) (store.Page, error) {
	if err != nil {
		return store.Page{}, err
	}
	for _, object := range page.Objects {
		if !r.within(object.Key) {
			return store.Page{}, errStoreConfinement
		}
	}
	return page, nil
}

func (r *multiPrefixReader) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	if !r.within(request.Key) {
		return nil, errStoreConfinement
	}
	return r.reader.Open(ctx, request)
}

func (r *multiPrefixReader) Stat(ctx context.Context, request store.StatRequest) (store.Object, error) {
	if !r.within(request.Key) {
		return store.Object{}, errStoreConfinement
	}
	object, err := r.reader.Stat(ctx, request)
	if err != nil {
		return store.Object{}, err
	}
	if !r.within(object.Key) {
		return store.Object{}, errStoreConfinement
	}
	return object, nil
}
