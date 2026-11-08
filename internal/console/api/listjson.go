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

package api

import (
	"bufio"
	"bytes"
	"io"
	"slices"
	"strconv"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// jsonFormat is huma.DefaultJSONFormat, except for the cluster list:
// its rows are copied from the encodings the read model memoized with
// them (viewmodel.ClusterRow.WithJSON) instead of encoded again on every
// request. At 1,600 Clusters the list's encoding was 29 % of the
// console's CPU under load (2026-11-03); a memoized row's encoding is
// now built once per change of the row, not once per request.
//
// The bytes written are the bytes huma.DefaultJSONFormat writes for the
// same value (TestClusterListJSONMatchesTheDefaultFormat): same field
// order, same escaping (HTML escaping off), same trailing newline.
var jsonFormat = huma.Format{
	Marshal: func(w io.Writer, v any) error {
		switch l := v.(type) {
		case ClusterListView:
			return l.writeJSON(w)
		case *ClusterListView:
			if l != nil {
				return l.writeJSON(w)
			}
		}
		return huma.DefaultJSONFormat.Marshal(w, v)
	},
	Unmarshal: huma.DefaultJSONFormat.Unmarshal,
}

// listBuffers holds the scratch buffers of writeJSON: the small head of
// the document and the encodings of rows that came without one.
var listBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// listWriters are the buffered writers rows are copied through, so a
// 2 MiB list is neither assembled in one allocation nor written to the
// connection in 1,600 small writes.
var listWriters = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 32<<10) }}

// maxPooledBuffer keeps an outsized scratch buffer out of the pool.
const maxPooledBuffer = 1 << 20

func putBuffer(b *bytes.Buffer) {
	if b.Cap() <= maxPooledBuffer {
		b.Reset()
		listBuffers.Put(b)
	}
}

// writeJSON writes v as encoding/json would, field by field: the
// ClusterListView document is {"generatedAt","namespaces","items",
// "unchecked" (omitempty)}, pinned by TestClusterListViewFields. Every
// value that can fail to encode is encoded before the first byte is
// written, so a failure writes nothing, as the default format does.
func (v ClusterListView) writeJSON(w io.Writer) error {
	if !slices.ContainsFunc(v.Items, func(r viewmodel.ClusterRow) bool { return r.EncodedJSON() != nil }) {
		// Nothing memoized (a read model without change stamps): one
		// pass of the encoder beats an encoder per row.
		return huma.DefaultJSONFormat.Marshal(w, v)
	}
	head := listBuffers.Get().(*bytes.Buffer)
	defer putBuffer(head)
	head.WriteString(`{"generatedAt":`)
	if err := viewmodel.AppendJSON(head, v.GeneratedAt); err != nil {
		return err
	}
	head.WriteString(`,"namespaces":`)
	if err := viewmodel.AppendJSON(head, v.Namespaces); err != nil {
		return err
	}
	head.WriteString(`,"items":`)

	// Rows without a memoized encoding (a read model without change
	// stamps, an unread Cluster's row) are encoded here; ends[i] is
	// where row i ends in extra.
	extra := listBuffers.Get().(*bytes.Buffer)
	defer putBuffer(extra)
	var ends []int
	for i := range v.Items {
		if v.Items[i].EncodedJSON() != nil {
			continue
		}
		if ends == nil {
			ends = make([]int, len(v.Items))
		}
		if err := viewmodel.AppendJSON(extra, v.Items[i]); err != nil {
			return err
		}
		ends[i] = extra.Len()
	}

	bw := listWriters.Get().(*bufio.Writer)
	bw.Reset(w)
	defer func() {
		bw.Reset(nil)
		listWriters.Put(bw)
	}()
	_, _ = bw.Write(head.Bytes())
	if v.Items == nil {
		_, _ = bw.WriteString("null")
	} else {
		_ = bw.WriteByte('[')
		start := 0
		for i := range v.Items {
			if i > 0 {
				_ = bw.WriteByte(',')
			}
			if b := v.Items[i].EncodedJSON(); b != nil {
				_, _ = bw.Write(b)
				continue
			}
			_, _ = bw.Write(extra.Bytes()[start:ends[i]])
			start = ends[i]
		}
		_ = bw.WriteByte(']')
	}
	if v.Unchecked != 0 {
		_, _ = bw.WriteString(`,"unchecked":`)
		_, _ = bw.WriteString(strconv.Itoa(v.Unchecked))
	}
	_, _ = bw.WriteString("}\n")
	// A bufio.Writer keeps its first write error and returns it here.
	return bw.Flush()
}
