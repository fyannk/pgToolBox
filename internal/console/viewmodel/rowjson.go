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

package viewmodel

import (
	"bytes"
	"encoding/json"
)

// AppendJSON appends v encoded the way the API's JSON format encodes a
// response body (huma.DefaultJSONFormat: encoding/json with HTML
// escaping off), without the encoder's trailing newline. The value's
// encoding does not depend on where it sits in the document (no
// indentation), so a row encoded alone is byte for byte the row inside
// the encoded list; internal/console/api pins that equality in a test.
func AppendJSON(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode ends every value with '\n'.
	return nil
}

// WithJSON returns the row with its JSON encoding attached, for the list
// writer to copy instead of encoding the row on every request. The read
// model attaches it when it memoizes a row (status.rowMemo), so the
// encoding is invalidated by exactly what invalidates the row.
//
// The encoding is of the row's content only: a row carries no per-user
// field (authorization removes whole rows, after the memo), so one
// encoding serves every caller. A row changed after WithJSON must be
// passed through it again (or through WithoutJSON): the attached bytes
// would describe the old content. On an encoding error the row is
// returned without an encoding, and the writer's own encoding of it
// reports the error.
func (r ClusterRow) WithJSON() ClusterRow {
	r.encoded = nil
	var buf bytes.Buffer
	if err := AppendJSON(&buf, r); err != nil {
		return r
	}
	r.encoded = bytes.Clone(buf.Bytes()) // exact size: the memo keeps it
	return r
}

// WithoutJSON returns the row with no encoding attached.
func (r ClusterRow) WithoutJSON() ClusterRow {
	r.encoded = nil
	return r
}

// EncodedJSON is the encoding WithJSON attached, or nil. The bytes are
// shared by every copy of the row and must not be modified.
func (r ClusterRow) EncodedJSON() []byte { return r.encoded }
